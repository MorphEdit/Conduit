// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package cluster manages which sites belong together: identity, members,
// invites, joining, gossip and LAN discovery.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/config"
)

// DefaultStep leaves room for 10 sites: site n generates ids ending in n.
const DefaultStep = 10

// Identity is this site's row in conduit.node. Secret is this site's own
// credential; other sites only know its hash.
type Identity struct {
	ID        string
	ClusterID string
	Secret    string
	Offset    int64
	Step      int64
	Ready     bool
}

type Member struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Offset    int64     `json:"offset"`
	Status    string    `json:"status"` // active | removed
	KeyHash   string    `json:"key_hash"`
	CertFP    string    `json:"cert_fp"`
	JoinedAt  time.Time `json:"joined_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Setting is one cluster-wide key (e.g. "tables", "step").
type Setting struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// View is what sites exchange when gossiping.
type View struct {
	ClusterID string    `json:"cluster_id"`
	Members   []Member  `json:"members"`
	Settings  []Setting `json:"settings"`
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func LoadIdentity(ctx context.Context, pool *pgxpool.Pool) (*Identity, error) {
	var id Identity
	err := pool.QueryRow(ctx,
		`SELECT id, cluster_id, token, id_offset, id_step, ready FROM conduit.node`).
		Scan(&id.ID, &id.ClusterID, &id.Secret, &id.Offset, &id.Step, &id.Ready)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &id, err
}

func SaveIdentity(ctx context.Context, pool *pgxpool.Pool, id *Identity) error {
	_, err := pool.Exec(ctx, `
		INSERT INTO conduit.node (id, cluster_id, token, id_offset, id_step, ready) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (singleton) DO UPDATE SET id = EXCLUDED.id, cluster_id = EXCLUDED.cluster_id,
			token = EXCLUDED.token, id_offset = EXCLUDED.id_offset, id_step = EXCLUDED.id_step, ready = EXCLUDED.ready`,
		id.ID, id.ClusterID, id.Secret, id.Offset, id.Step, id.Ready)
	return err
}

// NewSecret returns a fresh per-site credential.
func NewSecret() string { return randomHex(24) }

// Found creates a brand-new cluster with this site as its first member.
func Found(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, certFP string) (*Identity, error) {
	id := &Identity{ID: cfg.NodeID, ClusterID: randomHex(8), Secret: NewSecret(), Offset: 1, Step: DefaultStep, Ready: true}
	now := time.Now()
	self := Member{ID: id.ID, URL: cfg.Advertise, Offset: 1, Status: "active", KeyHash: hashSecret(id.Secret), CertFP: certFP, JoinedAt: now, UpdatedAt: now}
	if err := UpsertMembers(ctx, pool, []Member{self}); err != nil {
		return nil, err
	}
	if err := PutSetting(ctx, pool, "step", DefaultStep); err != nil {
		return nil, err
	}
	return id, SaveIdentity(ctx, pool, id)
}

// Members returns every known member (active and removed).
func Members(ctx context.Context, pool *pgxpool.Pool) ([]Member, error) {
	rows, err := pool.Query(ctx, `SELECT id, url, id_offset, status, key_hash, cert_fp, joined_at, updated_at
		FROM conduit.members ORDER BY id_offset, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		return m, r.Scan(&m.ID, &m.URL, &m.Offset, &m.Status, &m.KeyHash, &m.CertFP, &m.JoinedAt, &m.UpdatedAt)
	})
}

// ErrUnknownSite means a request's credentials match no active member.
var ErrUnknownSite = errors.New("unknown, removed or wrongly authenticated site")

// Authenticate checks a site's id and secret against the member list.
// Removed members fail immediately, which is how removal revokes access.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, id, secret string) error {
	var keyHash string
	err := pool.QueryRow(ctx, `SELECT key_hash FROM conduit.members WHERE id = $1 AND status = 'active'`, id).Scan(&keyHash)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (keyHash == "" || !secretMatches(secret, keyHash))) {
		return ErrUnknownSite
	}
	return err
}

// UpsertMembers stores members, keeping whichever version is newer.
// It reports whether anything changed.
func UpsertMembers(ctx context.Context, pool *pgxpool.Pool, ms []Member) error {
	_, err := mergeMembers(ctx, pool, ms)
	return err
}

func mergeMembers(ctx context.Context, pool *pgxpool.Pool, ms []Member) (bool, error) {
	changed := false
	for _, m := range ms {
		if !config.ValidID(m.ID) || (m.Status != "active" && m.Status != "removed") {
			continue
		}
		tag, err := pool.Exec(ctx, `
			INSERT INTO conduit.members (id, url, id_offset, status, key_hash, cert_fp, joined_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (id) DO UPDATE SET url = EXCLUDED.url, id_offset = EXCLUDED.id_offset, status = EXCLUDED.status,
				key_hash = EXCLUDED.key_hash, cert_fp = EXCLUDED.cert_fp,
				joined_at = EXCLUDED.joined_at, updated_at = EXCLUDED.updated_at
			WHERE conduit.members.updated_at < EXCLUDED.updated_at`,
			m.ID, m.URL, m.Offset, m.Status, m.KeyHash, m.CertFP, m.JoinedAt, m.UpdatedAt)
		if err != nil {
			return changed, err
		}
		changed = changed || tag.RowsAffected() > 0
	}
	return changed, nil
}

func Settings(ctx context.Context, pool *pgxpool.Pool) ([]Setting, error) {
	rows, err := pool.Query(ctx, `SELECT key, value, updated_at FROM conduit.settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Setting, error) {
		var s Setting
		return s, r.Scan(&s.Key, &s.Value, &s.UpdatedAt)
	})
}

func PutSetting(ctx context.Context, pool *pgxpool.Pool, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = mergeSettings(ctx, pool, []Setting{{Key: key, Value: raw, UpdatedAt: time.Now()}})
	return err
}

func mergeSettings(ctx context.Context, pool *pgxpool.Pool, ss []Setting) (bool, error) {
	changed := false
	for _, s := range ss {
		tag, err := pool.Exec(ctx, `
			INSERT INTO conduit.settings (key, value, updated_at) VALUES ($1, $2, $3)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
			WHERE conduit.settings.updated_at < EXCLUDED.updated_at`, s.Key, []byte(s.Value), s.UpdatedAt)
		if err != nil {
			return changed, err
		}
		changed = changed || tag.RowsAffected() > 0
	}
	return changed, nil
}

// Merge folds a remote view into ours and reports whether anything changed.
func Merge(ctx context.Context, pool *pgxpool.Pool, v View) (bool, error) {
	a, err := mergeMembers(ctx, pool, v.Members)
	if err != nil {
		return a, err
	}
	b, err := mergeSettings(ctx, pool, v.Settings)
	return a || b, err
}

func LocalView(ctx context.Context, pool *pgxpool.Pool, clusterID string) (View, error) {
	ms, err := Members(ctx, pool)
	if err != nil {
		return View{}, err
	}
	ss, err := Settings(ctx, pool)
	return View{ClusterID: clusterID, Members: ms, Settings: ss}, err
}

// TablesSetting reads the shared table policies.
func TablesSetting(ctx context.Context, pool *pgxpool.Pool) (map[string]config.TablePolicy, bool, error) {
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT value FROM conduit.settings WHERE key = 'tables'`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var t map[string]config.TablePolicy
	return t, true, json.Unmarshal(raw, &t)
}

// allocate picks the lowest offset never used by any member (removed
// members keep theirs: rows they created still carry those ids) and adds
// the new member, all under a table lock so two joins cannot collide here.
func allocate(ctx context.Context, pool *pgxpool.Pool, req JoinRequest, step int64) (int64, error) {
	id := req.ID
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE conduit.members IN EXCLUSIVE MODE`); err != nil {
		return 0, err
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT status FROM conduit.members WHERE id = $1`, id).Scan(&existing)
	if err == nil {
		return 0, fmt.Errorf("%w: %q", ErrNameTaken, id)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id_offset FROM conduit.members`)
	if err != nil {
		return 0, err
	}
	used, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	taken := map[int64]bool{}
	for _, o := range used {
		taken[o] = true
	}
	var offset int64
	for o := int64(1); o <= step; o++ {
		if !taken[o] {
			offset = o
			break
		}
	}
	if offset == 0 {
		return 0, fmt.Errorf("cluster is full: all %d id slots are used", step)
	}
	now := time.Now()
	if _, err := tx.Exec(ctx,
		`INSERT INTO conduit.members (id, url, id_offset, status, key_hash, cert_fp, joined_at, updated_at)
		 VALUES ($1, $2, $3, 'active', $4, $5, $6, $6)`,
		id, strings.TrimRight(req.URL, "/"), offset, req.KeyHash, req.CertFP, now); err != nil {
		return 0, err
	}
	return offset, tx.Commit(ctx)
}

var ErrNameTaken = errors.New("site name already used in this cluster")

// WasRemoved reports whether id/secret belong to a member that has been
// removed, so it can be told to stop instead of just being refused.
func WasRemoved(ctx context.Context, pool *pgxpool.Pool, id, secret string) bool {
	var keyHash string
	err := pool.QueryRow(ctx, `SELECT key_hash FROM conduit.members WHERE id = $1 AND status = 'removed'`, id).Scan(&keyHash)
	return err == nil && keyHash != "" && secretMatches(secret, keyHash)
}

// Remove marks a member removed; the change spreads by gossip.
func Remove(ctx context.Context, pool *pgxpool.Pool, id string) error {
	tag, err := pool.Exec(ctx,
		`UPDATE conduit.members SET status = 'removed', updated_at = now() WHERE id = $1 AND status = 'active'`, id)
	if err == nil && tag.RowsAffected() == 0 {
		err = fmt.Errorf("no active member %q", id)
	}
	return err
}

// UpdateSelfURL refreshes our own member row when the advertised URL changes.
func UpdateSelfURL(ctx context.Context, pool *pgxpool.Pool, id, url string) error {
	_, err := pool.Exec(ctx,
		`UPDATE conduit.members SET url = $2, updated_at = now() WHERE id = $1 AND url <> $2`, id, url)
	return err
}
