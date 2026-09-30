// Package store keeps Conduit's own state (outbox, cursors, inbox) in the
// "conduit" schema of the node's Postgres database.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/change"
)

var bootstrapSQL = []string{
	`CREATE SCHEMA IF NOT EXISTS conduit`,
	// One row per captured source transaction. seq is the order peers apply in.
	`CREATE TABLE IF NOT EXISTS conduit.outbox (
		seq         BIGSERIAL PRIMARY KEY,
		lsn         PG_LSN      NOT NULL UNIQUE,
		commit_time TIMESTAMPTZ NOT NULL,
		payload     JSONB       NOT NULL,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// How far each peer has acknowledged our outbox.
	`CREATE TABLE IF NOT EXISTS conduit.peer_cursor (
		peer_id    TEXT PRIMARY KEY,
		acked_seq  BIGINT      NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// How far we have applied each origin's outbox. Updated in the same
	// transaction as the applied rows, which makes redelivery harmless.
	`CREATE TABLE IF NOT EXISTS conduit.inbox_state (
		origin      TEXT PRIMARY KEY,
		applied_seq BIGINT      NOT NULL DEFAULT 0,
		updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// Deleted-row markers: lets an old UPDATE that arrives after a newer
	// DELETE lose instead of resurrecting the row.
	`CREATE TABLE IF NOT EXISTS conduit.tombstones (
		schema_name TEXT        NOT NULL,
		table_name  TEXT        NOT NULL,
		pk          TEXT        NOT NULL,
		deleted_at  TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (schema_name, table_name, pk)
	)`,
	// Every remote change that was not applied as-is, for audit and manual fixes.
	`CREATE TABLE IF NOT EXISTS conduit.conflicts (
		id          BIGSERIAL PRIMARY KEY,
		detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		origin      TEXT        NOT NULL,
		schema_name TEXT        NOT NULL,
		table_name  TEXT        NOT NULL,
		pk          TEXT        NOT NULL,
		kind        TEXT        NOT NULL,
		resolution  TEXT        NOT NULL,
		remote_time TIMESTAMPTZ,
		change      JSONB       NOT NULL,
		detail      TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS conflicts_detected_at ON conduit.conflicts (detected_at)`,
	// This site's identity in the cluster (single row).
	`CREATE TABLE IF NOT EXISTS conduit.node (
		singleton  BOOLEAN PRIMARY KEY DEFAULT true CHECK (singleton),
		id         TEXT        NOT NULL,
		cluster_id TEXT        NOT NULL,
		token      TEXT        NOT NULL,
		id_offset  INT         NOT NULL,
		id_step    INT         NOT NULL,
		ready      BOOLEAN     NOT NULL DEFAULT false,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// Every site this one knows about. Exchanged by gossip, newest row wins.
	`CREATE TABLE IF NOT EXISTS conduit.members (
		id         TEXT PRIMARY KEY,
		url        TEXT        NOT NULL,
		id_offset  INT         NOT NULL,
		status     TEXT        NOT NULL DEFAULT 'active',
		joined_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// Cluster-wide settings (table owners, id step). Gossiped like members.
	`CREATE TABLE IF NOT EXISTS conduit.settings (
		key        TEXT PRIMARY KEY,
		value      JSONB       NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	// One-time invite codes issued by this site (only hashes are stored).
	`CREATE TABLE IF NOT EXISTS conduit.invites (
		secret_hash TEXT PRIMARY KEY,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at  TIMESTAMPTZ NOT NULL,
		used_at     TIMESTAMPTZ,
		used_by     TEXT
	)`,
	// Rejects writes to tables owned by another node. Apply sessions run with
	// session_replication_role = replica, where this trigger does not fire.
	`CREATE OR REPLACE FUNCTION conduit.owner_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		RAISE EXCEPTION 'table %.% is owned by node "%"; write there instead',
			TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_ARGV[0]
			USING ERRCODE = 'insufficient_privilege';
	END $$`,
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Bootstrap(ctx context.Context) error {
	for _, q := range bootstrapSQL {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// EnsureCursor creates the cursor row for a peer (idempotent).
func (s *Store) EnsureCursor(ctx context.Context, peer string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO conduit.peer_cursor (peer_id) VALUES ($1) ON CONFLICT DO NOTHING`, peer)
	return err
}

// DeleteCursor forgets a removed peer so it no longer holds back Trim.
func (s *Store) DeleteCursor(ctx context.Context, peer string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM conduit.peer_cursor WHERE peer_id = $1`, peer)
	return err
}

// AppendOutbox stores one committed transaction. The unique lsn makes a
// replay after a crash (before the slot position was confirmed) a no-op.
// Local deletes also leave a tombstone, written in the same transaction.
func (s *Store) AppendOutbox(ctx context.Context, lsn string, commitTime time.Time, changes []change.Change) error {
	payload, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO conduit.outbox (lsn, commit_time, payload) VALUES ($1::pg_lsn, $2, $3)
		 ON CONFLICT (lsn) DO NOTHING`, lsn, commitTime, payload); err != nil {
		return err
	}
	for _, ch := range changes {
		if ch.Op == "D" {
			if err := AddTombstone(ctx, tx, ch.Schema, ch.Table, change.KeyJSON(ch.RowKey()), commitTime); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// Execer is satisfied by pgx pools, connections and transactions.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func AddTombstone(ctx context.Context, db Execer, schema, table, pk string, at time.Time) error {
	_, err := db.Exec(ctx,
		`INSERT INTO conduit.tombstones (schema_name, table_name, pk, deleted_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (schema_name, table_name, pk)
		 DO UPDATE SET deleted_at = greatest(conduit.tombstones.deleted_at, EXCLUDED.deleted_at)`,
		schema, table, pk, at)
	return err
}

// Janitor removes tombstones older than ttl and conflicts older than 90 days.
func (s *Store) Janitor(ctx context.Context, ttl time.Duration) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM conduit.tombstones WHERE deleted_at < now() - make_interval(secs => $1)`, ttl.Seconds()); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM conduit.conflicts WHERE detected_at < now() - interval '90 days'`)
	return err
}

func (s *Store) ReadOutbox(ctx context.Context, afterSeq int64, limit int) ([]change.Tx, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT seq, lsn::text, commit_time, payload FROM conduit.outbox
		 WHERE seq > $1 ORDER BY seq LIMIT $2`, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var txs []change.Tx
	for rows.Next() {
		var tx change.Tx
		var payload []byte
		if err := rows.Scan(&tx.Seq, &tx.LSN, &tx.CommitTime, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &tx.Changes); err != nil {
			return nil, fmt.Errorf("outbox seq %d: %w", tx.Seq, err)
		}
		txs = append(txs, tx)
	}
	return txs, rows.Err()
}

func (s *Store) Cursor(ctx context.Context, peer string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT acked_seq FROM conduit.peer_cursor WHERE peer_id = $1`, peer).Scan(&seq)
	return seq, err
}

func (s *Store) SetCursor(ctx context.Context, peer string, seq int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE conduit.peer_cursor SET acked_seq = $2, updated_at = now()
		 WHERE peer_id = $1 AND acked_seq < $2`, peer, seq)
	return err
}

// Trim deletes outbox rows that every active member has acknowledged and
// that are older than retention. The retention window covers a site that is
// joining right now: other sites may not have heard of it yet, but it will
// still find everything after its snapshot here.
func (s *Store) Trim(ctx context.Context, self string, retention time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM conduit.outbox
		WHERE created_at < now() - make_interval(secs => $2)
		  AND seq <= coalesce((
			SELECT min(coalesce(c.acked_seq, 0))
			FROM conduit.members m
			LEFT JOIN conduit.peer_cursor c ON c.peer_id = m.id
			WHERE m.status = 'active' AND m.id <> $1), 9223372036854775807)`,
		self, retention.Seconds())
	return err
}

type OutboxStats struct {
	// Pending is how many transactions at least one peer still lacks
	// (filled in by the server from the peer cursors).
	Pending int64 `json:"pending"`
	// Retained is how many rows are kept, including delivered ones held
	// for the retention window.
	Retained int64 `json:"retained"`
	MaxSeq   int64 `json:"max_seq"`
}

func (s *Store) OutboxStats(ctx context.Context) (OutboxStats, error) {
	var st OutboxStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(*), (SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM conduit.outbox_seq_seq)
		 FROM conduit.outbox`).
		Scan(&st.Retained, &st.MaxSeq)
	return st, err
}

type InboxState struct {
	Origin     string    `json:"origin"`
	AppliedSeq int64     `json:"applied_seq"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (s *Store) InboxStates(ctx context.Context) ([]InboxState, error) {
	rows, err := s.pool.Query(ctx, `SELECT origin, applied_seq, updated_at FROM conduit.inbox_state ORDER BY origin`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxState
	for rows.Next() {
		var st InboxState
		if err := rows.Scan(&st.Origin, &st.AppliedSeq, &st.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

type Conflict struct {
	ID         int64      `json:"id"`
	DetectedAt time.Time  `json:"detected_at"`
	Origin     string     `json:"origin"`
	Table      string     `json:"table"`
	PK         string     `json:"pk"`
	Kind       string     `json:"kind"`
	Resolution string     `json:"resolution"`
	RemoteTime *time.Time `json:"remote_time,omitempty"`
	Detail     string     `json:"detail,omitempty"`
}

type ConflictStats struct {
	Total  int64      `json:"total"`
	Recent []Conflict `json:"recent"`
}

func (s *Store) Conflicts(ctx context.Context, limit int) (ConflictStats, error) {
	var st ConflictStats
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM conduit.conflicts`).Scan(&st.Total); err != nil {
		return st, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, detected_at, origin, schema_name || '.' || table_name, pk, kind, resolution, remote_time, coalesce(detail, '')
		 FROM conduit.conflicts ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	st.Recent = []Conflict{}
	for rows.Next() {
		var c Conflict
		if err := rows.Scan(&c.ID, &c.DetectedAt, &c.Origin, &c.Table, &c.PK, &c.Kind, &c.Resolution, &c.RemoteTime, &c.Detail); err != nil {
			return st, err
		}
		st.Recent = append(st.Recent, c)
	}
	return st, rows.Err()
}
