// Package store keeps Conduit's own state (outbox, cursors, inbox) in the
// "conduit" schema of the node's Postgres database.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
}

type Store struct {
	pool  *pgxpool.Pool
	peers []string
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

// EnsureCursors creates a cursor row for every peer so Trim never deletes
// rows a peer has not seen yet.
func (s *Store) EnsureCursors(ctx context.Context, peers []string) error {
	s.peers = peers
	for _, p := range peers {
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO conduit.peer_cursor (peer_id) VALUES ($1) ON CONFLICT DO NOTHING`, p); err != nil {
			return err
		}
	}
	return nil
}

// AppendOutbox stores one committed transaction. The unique lsn makes a
// replay after a crash (before the slot position was confirmed) a no-op.
func (s *Store) AppendOutbox(ctx context.Context, lsn string, commitTime time.Time, changes []change.Change) error {
	payload, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO conduit.outbox (lsn, commit_time, payload) VALUES ($1::pg_lsn, $2, $3)
		 ON CONFLICT (lsn) DO NOTHING`, lsn, commitTime, payload)
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

// Trim deletes outbox rows every configured peer has acknowledged.
func (s *Store) Trim(ctx context.Context) error {
	if len(s.peers) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM conduit.outbox WHERE seq <= (
			SELECT min(acked_seq) FROM conduit.peer_cursor WHERE peer_id = ANY($1))`, s.peers)
	return err
}

type OutboxStats struct {
	Pending int64 `json:"pending"`
	MaxSeq  int64 `json:"max_seq"`
}

func (s *Store) OutboxStats(ctx context.Context) (OutboxStats, error) {
	var st OutboxStats
	err := s.pool.QueryRow(ctx,
		`SELECT count(*), (SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM conduit.outbox_seq_seq)
		 FROM conduit.outbox`).
		Scan(&st.Pending, &st.MaxSeq)
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
