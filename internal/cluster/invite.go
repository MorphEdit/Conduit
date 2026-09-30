package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const invitePrefix = "cdt1_"

// Invite is what a code carries: where to join and a one-time secret.
type Invite struct {
	URL    string `json:"u"`
	Secret string `json:"s"`
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
	if err != nil || i.URL == "" || i.Secret == "" {
		return i, errors.New("invite code is damaged; copy it again")
	}
	return i, nil
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// CreateInvite issues a one-time code that lets one new site join through
// this site. It stays valid for ttl.
func CreateInvite(ctx context.Context, pool *pgxpool.Pool, selfURL string, ttl time.Duration) (string, time.Time, error) {
	secret := randomHex(24)
	expires := time.Now().Add(ttl)
	_, err := pool.Exec(ctx, `INSERT INTO conduit.invites (secret_hash, expires_at) VALUES ($1, $2)`, hashSecret(secret), expires)
	return Invite{URL: selfURL, Secret: secret}.Encode(), expires, err
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

// JoinRequest is sent by a new site to redeem an invite.
type JoinRequest struct {
	Secret string `json:"secret"`
	ID     string `json:"id"`
	URL    string `json:"url"`
}

// JoinResponse hands the new site everything it needs.
type JoinResponse struct {
	ClusterID string `json:"cluster_id"`
	Token     string `json:"token"`
	Offset    int64  `json:"offset"`
	Step      int64  `json:"step"`
	Seed      string `json:"seed"` // the admitting site's id
	View      View   `json:"view"`
}

// Admit handles a join on the admitting (seed) site.
func Admit(ctx context.Context, pool *pgxpool.Pool, self *Identity, req JoinRequest) (*JoinResponse, error) {
	if err := redeem(ctx, pool, req.Secret, req.ID); err != nil {
		return nil, err
	}
	offset, err := allocate(ctx, pool, req.ID, strings.TrimRight(req.URL, "/"), self.Step)
	if err != nil {
		unredeem(ctx, pool, req.Secret)
		return nil, err
	}
	view, err := LocalView(ctx, pool, self.ClusterID)
	if err != nil {
		return nil, err
	}
	return &JoinResponse{ClusterID: self.ClusterID, Token: self.Token, Offset: offset, Step: self.Step, Seed: self.ID, View: view}, nil
}

func (r *JoinResponse) String() string {
	return fmt.Sprintf("cluster %s via %s, id offset %d/%d", r.ClusterID, r.Seed, r.Offset, r.Step)
}
