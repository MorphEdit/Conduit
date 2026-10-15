// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"filippo.io/cpace"

	"github.com/MorphEdit/conduit/internal/apply"
	"github.com/MorphEdit/conduit/internal/cluster"
	"github.com/MorphEdit/conduit/internal/peer"
	"github.com/MorphEdit/conduit/internal/pgcli"
	"github.com/MorphEdit/conduit/internal/snapshot"
	"github.com/MorphEdit/conduit/internal/store"
)

const peerTimeout = 15 * time.Second

var createSchema = regexp.MustCompile(`(?m)^CREATE SCHEMA (\S+);`)

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// join waits for a way in (invite from config, invite pasted on the
// dashboard, or approval of a LAN join request) and then joins.
func (r *Runtime) join(ctx context.Context, st *store.Store) (*cluster.Identity, error) {
	cfg := r.Cfg
	code := cfg.Join
	pairing := &Pairing{Code: pairingCode(), Status: "searching"}
	r.set(func() { r.pairing = pairing })
	r.setPhase(PhaseWaitingJoin, "")
	// The log is the only way to read the code on a site without an admin
	// password. With one, the code stays on the dashboard: logs are often
	// collected and readable by more people than the admin.
	logCode := func(code string) string {
		if cfg.AdminPassword != "" {
			return "(shown on this site's dashboard)"
		}
		return code
	}

	var lan *cluster.Listener
	if cfg.Discovery && code == "" {
		lan = cluster.Listen(ctx, r.Log)
		r.Log.Info("waiting to join: approve this site on any running site's dashboard, or paste an invite", "pairing_code", logCode(pairing.Code))
	}
	reqSecret := randHex(16)
	var reqID string
	var seed peer.Target
	var pake *cpace.State
	var quietUntil time.Time
	badApprovals := 0

	for {
		if code != "" {
			id, err := r.redeem(ctx, st, code)
			if err == nil {
				return id, nil
			}
			r.setErr(err)
			r.Log.Warn("join failed", "err", err)
			if errors.Is(err, cluster.ErrBadInvite) || errors.Is(err, peer.ErrFingerprint) {
				code = "" // wait for a new one
			}
		} else if lan != nil && time.Now().After(quietUntil) {
			if reqID == "" {
				if b, ok := lan.Best(); ok && b.FP != "" {
					t := peer.Target{URL: b.URL, Fingerprint: b.FP}
					if id, st, err := r.requestJoin(ctx, t, r.View().Pairing.Code, reqSecret); err == nil {
						reqID, seed, pake = id, t, st
						r.set(func() { pairing.SeedID, pairing.Status = b.ID, "requested" })
						r.Log.Info("asked to join; approve it on the dashboard", "site", b.ID, "pairing_code", logCode(pairing.Code))
					} else {
						r.setErr(err)
					}
				}
			} else {
				status, reply, box, err := r.pollJoin(ctx, seed, reqID, reqSecret)
				var invite string
				if err == nil && status == "approved" {
					invite, err = cluster.PairingOpen(pake, reply, box, reqID)
				}
				switch {
				case errors.Is(err, cluster.ErrBadPairing):
					// Whoever approved did not know the code shown on this
					// site: a mistyped code, or a fake site on the LAN. Never
					// use what it sent; start over with a fresh code, waiting
					// longer each time so a fake site gets few guesses.
					badApprovals++
					reqID, pake = "", nil
					quietUntil = time.Now().Add(min(20*time.Second<<min(badApprovals-1, 5), 10*time.Minute))
					next := pairingCode()
					r.set(func() { pairing.Code, pairing.Status = next, "bad_proof" })
					r.Log.Warn("join approval did not prove the pairing code; ignored it", "site", pairing.SeedID, "new_pairing_code", logCode(next))
				case err != nil:
					reqID, pake = "", nil // the other site restarted; ask again
				case status == "approved":
					code = invite
					continue
				case status == "rejected":
					reqID, pake, quietUntil = "", nil, time.Now().Add(5*time.Minute)
					r.set(func() { pairing.Status = "rejected" })
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case p := <-r.pasted:
			code = p
		case <-time.After(3 * time.Second):
		}
	}
}

// requestJoin asks a site found on the LAN to let this one in. The pairing
// code itself is never sent: the request carries this site's half of a PAKE
// keyed by it, which the approving admin answers by typing the code.
func (r *Runtime) requestJoin(ctx context.Context, seed peer.Target, code, secret string) (string, *cpace.State, error) {
	msg, st, err := cluster.PairingStart(code, r.Cfg.NodeID, seed.Fingerprint)
	if err != nil {
		return "", nil, err
	}
	body, _ := json.Marshal(map[string]any{"node_id": r.Cfg.NodeID, "url": r.Cfg.Advertise, "secret": secret, "pake": msg})
	var out struct {
		RequestID string `json:"request_id"`
	}
	if err := call(ctx, seed, peer.Credentials{}, http.MethodPost, "/v1/join-requests", body, nil, &out); err != nil {
		return "", nil, err
	}
	return out.RequestID, st, nil
}

func (r *Runtime) pollJoin(ctx context.Context, seed peer.Target, id, secret string) (status string, reply, box []byte, err error) {
	var out struct {
		Status string
		Pake   []byte
		Box    []byte
	}
	err = call(ctx, seed, peer.Credentials{}, http.MethodGet, "/v1/join-requests/"+id, nil,
		map[string]string{"X-Request-Secret": secret}, &out)
	return out.Status, out.Pake, out.Box, err
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("%d: %s", e.code, e.msg) }

// call makes one pinned request to another site and decodes a JSON reply.
func call(ctx context.Context, t peer.Target, creds peer.Credentials, method, path string, body []byte, headers map[string]string, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.URL, "/")+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if creds.ID != "" {
		req.Header.Set("Authorization", creds.Header())
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := peer.PinnedClient(t.Fingerprint, peerTimeout).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// redeem trades an invite code for membership, then copies schema and data.
// Only the hash of this site's new secret leaves this machine.
func (r *Runtime) redeem(ctx context.Context, st *store.Store, code string) (*cluster.Identity, error) {
	inv, err := cluster.DecodeInvite(code)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", cluster.ErrBadInvite, err)
	}
	r.setPhase(PhaseJoining, "กำลังขอเข้าร่วม")
	seed := peer.Target{URL: inv.URL, Fingerprint: inv.Fingerprint}
	secret := cluster.NewSecret()
	name := r.Cfg.NodeID
	var resp cluster.JoinResponse
	for attempt := 0; ; attempt++ {
		body, _ := json.Marshal(cluster.JoinRequest{
			Secret: inv.Secret, ID: name, URL: r.Cfg.Advertise,
			KeyHash: cluster.HashSecret(secret), CertFP: r.View().TLS.Fingerprint,
		})
		err = call(ctx, seed, peer.Credentials{}, http.MethodPost, "/v1/join", body, nil, &resp)
		var he *httpError
		if errors.As(err, &he) && he.code == http.StatusConflict && attempt == 0 {
			name = r.Cfg.NodeID + "_" + randHex(2) // name taken: pick a variant once
			continue
		}
		break
	}
	var he *httpError
	if errors.As(err, &he) && he.code == http.StatusForbidden {
		r.setPhase(PhaseWaitingJoin, "")
		return nil, fmt.Errorf("%w (%s)", cluster.ErrBadInvite, he.msg)
	}
	if err != nil {
		r.setPhase(PhaseWaitingJoin, "")
		return nil, err
	}
	r.Log.Info("admitted to cluster", "as", name, "detail", resp.String())

	id := &cluster.Identity{ID: name, ClusterID: resp.ClusterID, Secret: secret, Offset: resp.Offset, Step: resp.Step}
	if _, err := cluster.Merge(ctx, st.Pool(), resp.View, name, resp.Seed); err != nil {
		return nil, err
	}
	if err := cluster.SaveIdentity(ctx, st.Pool(), id); err != nil {
		return nil, err
	}
	return r.finishJoin(ctx, st, id, resp.Seed, false)
}

// finishJoin copies the table structure (if this database is empty) and all
// rows from one existing site, then marks this site ready.
func (r *Runtime) finishJoin(ctx context.Context, st *store.Store, id *cluster.Identity, seedID string, truncate bool) (*cluster.Identity, error) {
	cfg, pool := r.Cfg, st.Pool()
	cfg.NodeID = id.ID
	creds := peer.Credentials{ID: id.ID, Secret: id.Secret}

	members, err := cluster.Members(ctx, pool)
	if err != nil {
		return nil, err
	}
	var seed *cluster.Member
	for i, m := range members {
		if m.ID != id.ID && m.Status == "active" && (m.ID == seedID || (seedID == "" && seed == nil)) {
			seed = &members[i]
		}
	}
	if seed == nil {
		return nil, errors.New("no other site to copy data from")
	}
	target := peer.Target{URL: seed.URL, Fingerprint: seed.CertFP}
	r.setPhase(PhaseJoining, "กำลังคัดลอกข้อมูลจาก "+seed.ID)

	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = ANY($1)`, cfg.Capture.Schemas).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		if err := retry(ctx, r, func() error { return r.copySchema(ctx, target, creds) }); err != nil {
			return nil, err
		}
		r.Log.Info("copied table structure", "from", seed.ID)
	}

	ap := apply.New(cfg, r.Log)
	defer ap.Close()
	first := true
	if err := retry(ctx, r, func() error {
		t := truncate || !first
		first = false
		return snapshot.Pull(ctx, cfg, seed.ID, target, creds, t, ap, r.Log)
	}); err != nil {
		return nil, err
	}
	id.Ready = true
	return id, cluster.SaveIdentity(ctx, pool, id)
}

// copySchema fetches the peer's DDL (pg_dump --schema-only) and runs it here.
func (r *Runtime) copySchema(ctx context.Context, t peer.Target, creds peer.Credentials) error {
	resp, err := peer.Do(ctx, t, creds, http.MethodGet, "/v1/schema", nil, time.Minute)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	ddl, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("schema from peer: %s: %s", resp.Status, bytes.TrimSpace(ddl))
	}
	// The target already has "public"; make every CREATE SCHEMA a no-op if present.
	ddl = createSchema.ReplaceAll(ddl, []byte("CREATE SCHEMA IF NOT EXISTS $1;"))
	dbname, env := pgcli.Conn(r.Cfg.Database) // password goes in the environment, not the process list
	cmd := exec.CommandContext(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", dbname, "-f", "-")
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(ddl)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply schema: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
