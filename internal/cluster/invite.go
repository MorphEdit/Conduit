// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MorphEdit/conduit/internal/peer"
)

const invitePrefix = "cdt1_"

// Invite is what a code carries: where to join and a one-time secret.
type Invite struct {
	URL         string `json:"u"`
	Secret      string `json:"s"`
	Fingerprint string `json:"f"` // the issuing site's certificate, pinned by the new site
}

func (i Invite) Encode() string {
	raw, _ := json.Marshal(i)
	return invitePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

func DecodeInvite(code string) (Invite, error) {
	var i Invite
	code = strings.TrimSpace(code)
	if !strings.HasPrefix(code, invitePrefix) {
		return i, errors.New("not a Conduit invite code (should start with " + invitePrefix + ")")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(code, invitePrefix))
	if err == nil {
		err = json.Unmarshal(raw, &i)
	}
	if err != nil || i.URL == "" || i.Secret == "" || i.Fingerprint == "" {
		return i, errors.New("invite code is damaged; copy it again")
	}
	return i, nil
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// HashSecret is what a site publishes about its own secret.
func HashSecret(s string) string { return hashSecret(s) }

func secretMatches(secret, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(hash)) == 1
}

// CreateInvite issues a one-time code that lets one new site join through
// this site. It stays valid for ttl.
func CreateInvite(ctx context.Context, pool *pgxpool.Pool, selfURL, selfFP string, ttl time.Duration) (string, time.Time, error) {
	secret := randomHex(24)
	expires := time.Now().Add(ttl)
	_, err := pool.Exec(ctx, `INSERT INTO conduit.invites (secret_hash, expires_at) VALUES ($1, $2)`, hashSecret(secret), expires)
	return Invite{URL: selfURL, Secret: secret, Fingerprint: selfFP}.Encode(), expires, err
}

var ErrBadInvite = errors.New("invite code is invalid, expired or already used")

// redeem consumes an invite secret for the named site.
func redeem(ctx context.Context, pool *pgxpool.Pool, secret, by string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE conduit.invites SET used_at = now(), used_by = $2
		WHERE secret_hash = $1 AND used_at IS NULL AND expires_at > now()`, hashSecret(secret), by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrBadInvite
	}
	return nil
}

// unredeem gives an invite back when the join failed after redeeming it.
func unredeem(ctx context.Context, pool *pgxpool.Pool, secret string) {
	pool.Exec(ctx, `UPDATE conduit.invites SET used_at = NULL, used_by = NULL WHERE secret_hash = $1`, hashSecret(secret))
}

// JoinRequest is sent by a new site to redeem an invite. It publishes the
// new site's credential hash and certificate fingerprint, never the secret.
type JoinRequest struct {
	Secret  string `json:"secret"` // the invite's one-time secret
	ID      string `json:"id"`
	URL     string `json:"url"`
	KeyHash string `json:"key_hash"`
	CertFP  string `json:"cert_fp"`
}

// JoinResponse hands the new site everything it needs.
type JoinResponse struct {
	ClusterID string `json:"cluster_id"`
	Offset    int64  `json:"offset"`
	Step      int64  `json:"step"`
	Seed      string `json:"seed"` // the admitting site's id
	View      View   `json:"view"`
}

// Admit handles a join on the admitting (seed) site. The id slot always comes
// from the allocator (see Allocator); creds sign the request when that is
// another site.
func Admit(ctx context.Context, pool *pgxpool.Pool, self *Identity, creds peer.Credentials, req JoinRequest) (*JoinResponse, error) {
	if err := redeem(ctx, pool, req.Secret, req.ID); err != nil {
		return nil, err
	}
	offset, err := assign(ctx, pool, self, creds, req)
	if err != nil {
		unredeem(ctx, pool, req.Secret)
		return nil, err
	}
	view, err := LocalView(ctx, pool, self.ClusterID)
	if err != nil {
		return nil, err
	}
	return &JoinResponse{ClusterID: self.ClusterID, Offset: offset, Step: self.Step, Seed: self.ID, View: view}, nil
}

func assign(ctx context.Context, pool *pgxpool.Pool, self *Identity, creds peer.Credentials, req JoinRequest) (int64, error) {
	a, err := Allocator(ctx, pool)
	if err != nil {
		return 0, err
	}
	if a.ID == self.ID {
		return allocate(ctx, pool, req, self.Step)
	}
	if taken, err := nameTaken(ctx, pool, req.ID); err != nil || taken {
		if err == nil {
			err = fmt.Errorf("%w: %q", ErrNameTaken, req.ID)
		}
		return 0, err
	}
	offset, err := remoteAllocate(ctx, *a, creds, req)
	if err != nil {
		return 0, err
	}
	return offset, adopt(ctx, pool, req, offset)
}

// remoteAllocate asks the allocator for a slot. The invite secret stays here.
func remoteAllocate(ctx context.Context, a Member, creds peer.Credentials, req JoinRequest) (int64, error) {
	req.Secret = ""
	body, _ := json.Marshal(req)
	resp, err := peer.Do(ctx, peer.Target{URL: a.URL, Fingerprint: a.CertFP}, creds,
		http.MethodPost, "/v1/allocate", bytes.NewReader(body), 15*time.Second)
	if err != nil {
		return 0, fmt.Errorf("%s hands out id slots and cannot be reached right now (%v); try again when it is online", a.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusConflict && strings.Contains(string(msg), ErrNameTaken.Error()) {
			return 0, fmt.Errorf("%w: %q", ErrNameTaken, req.ID)
		}
		return 0, fmt.Errorf("%s could not assign an id slot: %s %s", a.ID, resp.Status, bytes.TrimSpace(msg))
	}
	var out struct {
		Offset int64 `json:"offset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Offset < 1 {
		return 0, fmt.Errorf("%s sent a bad id slot answer", a.ID)
	}
	return out.Offset, nil
}

func (r *JoinResponse) String() string {
	return fmt.Sprintf("cluster %s via %s, id offset %d/%d", r.ClusterID, r.Seed, r.Offset, r.Step)
}
