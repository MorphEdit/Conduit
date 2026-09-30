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

	"github.com/conduit-sync/conduit/internal/apply"
	"github.com/conduit-sync/conduit/internal/cluster"
	"github.com/conduit-sync/conduit/internal/snapshot"
	"github.com/conduit-sync/conduit/internal/store"
)

var (
	httpc        = &http.Client{Timeout: 15 * time.Second}
	createSchema = regexp.MustCompile(`(?m)^CREATE SCHEMA (\S+);`)
)

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

	var lan *cluster.Listener
	if cfg.Discovery && code == "" {
		lan = cluster.Listen(ctx, r.Log)
		r.Log.Info("waiting to join: approve this site on any running site's dashboard, or paste an invite", "pairing_code", pairing.Code)
	}
	reqSecret := randHex(16)
	var reqID, reqURL string
	var quietUntil time.Time

	for {
		if code != "" {
			id, err := r.redeem(ctx, st, code)
			if err == nil {
				return id, nil
			}
			r.setErr(err)
			r.Log.Warn("join failed", "err", err)
			if errors.Is(err, cluster.ErrBadInvite) {
				code = "" // wait for a new one
			}
		} else if lan != nil && time.Now().After(quietUntil) {
			if reqID == "" {
				if b, ok := lan.Best(); ok {
					if id, err := r.requestJoin(ctx, b.URL, pairing.Code, reqSecret); err == nil {
						reqID, reqURL = id, b.URL
						r.set(func() { pairing.SeedID, pairing.Status = b.ID, "requested" })
						r.Log.Info("asked to join; approve it on the dashboard", "site", b.ID, "pairing_code", pairing.Code)
					} else {
						r.setErr(err)
					}
				}
			} else {
				status, invite, err := r.pollJoin(ctx, reqURL, reqID, reqSecret)
				switch {
				case err != nil:
					reqID = "" // the other site restarted; ask again
				case status == "approved":
					code = invite
					continue
				case status == "rejected":
					reqID, quietUntil = "", time.Now().Add(5*time.Minute)
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

func (r *Runtime) requestJoin(ctx context.Context, url, code, secret string) (string, error) {
	body, _ := json.Marshal(map[string]string{"node_id": r.Cfg.NodeID, "url": r.Cfg.Advertise, "code": code, "secret": secret})
	var out struct {
		RequestID string `json:"request_id"`
	}
	return out.RequestID, post(ctx, url+"/v1/join-requests", "", body, &out)
}

func (r *Runtime) pollJoin(ctx context.Context, url, id, secret string) (string, string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/join-requests/"+id, nil)
	req.Header.Set("X-Request-Secret", secret)
	resp, err := httpc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("status %s", resp.Status)
	}
	var out struct{ Status, Invite string }
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out.Status, out.Invite, err
}

func post(ctx context.Context, url, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &httpError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("%d: %s", e.code, e.msg) }

// redeem trades an invite code for membership, then copies schema and data.
func (r *Runtime) redeem(ctx context.Context, st *store.Store, code string) (*cluster.Identity, error) {
	inv, err := cluster.DecodeInvite(code)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", cluster.ErrBadInvite, err)
	}
	r.setPhase(PhaseJoining, "กำลังขอเข้าร่วม")
	name := r.Cfg.NodeID
	var resp cluster.JoinResponse
	for attempt := 0; ; attempt++ {
		body, _ := json.Marshal(cluster.JoinRequest{Secret: inv.Secret, ID: name, URL: r.Cfg.Advertise})
		err = post(ctx, strings.TrimRight(inv.URL, "/")+"/v1/join", "", body, &resp)
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

	id := &cluster.Identity{ID: name, ClusterID: resp.ClusterID, Token: resp.Token, Offset: resp.Offset, Step: resp.Step}
	if _, err := cluster.Merge(ctx, st.Pool(), resp.View); err != nil {
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
	cfg.NodeID, cfg.Token = id.ID, id.Token

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
	r.setPhase(PhaseJoining, "กำลังคัดลอกข้อมูลจาก "+seed.ID)

	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = ANY($1)`, cfg.Capture.Schemas).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		if err := retry(ctx, r, func() error { return r.copySchema(ctx, seed.URL, id.Token) }); err != nil {
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
		return snapshot.Pull(ctx, cfg, seed.ID, seed.URL, id.Token, t, ap, r.Log)
	}); err != nil {
		return nil, err
	}
	id.Ready = true
	return id, cluster.SaveIdentity(ctx, pool, id)
}

// copySchema fetches the peer's DDL (pg_dump --schema-only) and runs it here.
func (r *Runtime) copySchema(ctx context.Context, url, token string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/schema", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := httpc.Do(req)
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
	cmd := exec.CommandContext(ctx, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", r.Cfg.Database, "-f", "-")
	cmd.Stdin = bytes.NewReader(ddl)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply schema: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
