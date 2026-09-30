// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package apply writes a peer's transactions into the local database and
// resolves conflicts with last-write-wins on commit timestamps.
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/conduit-sync/conduit/internal/change"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/store"
)

// Applier owns one dedicated connection, tagged with a replication origin
// per peer. Rows written through it:
//   - are skipped by the local capture ("origin 'none'"), so nothing loops;
//   - carry the ORIGIN's commit timestamp (pg_replication_origin_xact_setup),
//     so every node compares the same timestamps and converges.
type Applier struct {
	cfg *config.Config
	log *slog.Logger

	mu     sync.Mutex
	conn   *pgx.Conn
	origin string
}

func New(cfg *config.Config, log *slog.Logger) *Applier {
	return &Applier{cfg: cfg, log: log.With("component", "apply")}
}

func (a *Applier) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reset()
}

// Apply applies txs in seq order, skipping any already applied, and reports
// the highest seq that is now durably applied.
func (a *Applier) Apply(ctx context.Context, b change.Batch) change.Ack {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.ensure(ctx, b.Origin); err != nil {
		a.reset()
		return change.Ack{Error: err.Error()}
	}
	var applied int64
	err := a.conn.QueryRow(ctx,
		`SELECT coalesce((SELECT applied_seq FROM conduit.inbox_state WHERE origin = $1), 0)`, b.Origin).Scan(&applied)
	if err != nil {
		a.reset()
		return change.Ack{Error: err.Error()}
	}

	for _, tx := range b.Txs {
		if tx.Seq <= applied {
			continue
		}
		if err := a.applyTx(ctx, b.Origin, tx); err != nil {
			if a.conn.IsClosed() {
				a.reset()
			}
			return change.Ack{Applied: applied, Error: fmt.Sprintf("seq %d: %v", tx.Seq, err)}
		}
		applied = tx.Seq
	}
	return change.Ack{Applied: applied}
}

func (a *Applier) ensure(ctx context.Context, origin string) error {
	if a.conn == nil || a.conn.IsClosed() {
		conn, err := pgx.Connect(ctx, a.cfg.Database)
		if err != nil {
			return err
		}
		// Like Postgres' own apply workers: don't fire user triggers (audit
		// logs, owner guards) a second time and don't enforce FK order.
		if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
			conn.Close(ctx)
			return err
		}
		a.conn, a.origin = conn, ""
	}
	if a.origin == origin {
		return nil
	}
	if a.origin != "" {
		if _, err := a.conn.Exec(ctx, `SELECT pg_replication_origin_session_reset()`); err != nil {
			return err
		}
	}
	name := "conduit_" + origin
	if _, err := a.conn.Exec(ctx,
		`SELECT pg_replication_origin_create($1)
		 WHERE NOT EXISTS (SELECT 1 FROM pg_replication_origin WHERE roname = $1)`, name); err != nil {
		return err
	}
	if _, err := a.conn.Exec(ctx, `SELECT pg_replication_origin_session_setup($1)`, name); err != nil {
		return err
	}
	a.origin = origin
	return nil
}

func (a *Applier) reset() {
	if a.conn != nil {
		a.conn.Close(context.Background())
	}
	a.conn, a.origin = nil, ""
}

// begin opens a transaction stamped with the origin's LSN and commit time.
func (a *Applier) begin(ctx context.Context, lsn string, at time.Time) (pgx.Tx, error) {
	tx, err := a.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_replication_origin_xact_setup($1::pg_lsn, $2)`, lsn, at); err != nil {
		tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func (a *Applier) applyTx(ctx context.Context, origin string, t change.Tx) error {
	tx, err := a.begin(ctx, t.LSN, t.CommitTime)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for i, ch := range t.Changes {
		r := &row{origin: origin, at: t.CommitTime, ch: ch}
		if err := a.applyChange(ctx, tx, r); err != nil {
			return fmt.Errorf("change %d (%s %s.%s): %w", i, ch.Op, ch.Schema, ch.Table, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO conduit.inbox_state (origin, applied_seq, updated_at) VALUES ($1, $2, now())
		 ON CONFLICT (origin) DO UPDATE SET applied_seq = EXCLUDED.applied_seq, updated_at = now()`,
		origin, t.Seq); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// row is one remote change in flight.
type row struct {
	origin string
	at     time.Time
	ch     change.Change
}

func (r *row) table() string { return pgx.Identifier{r.ch.Schema, r.ch.Table}.Sanitize() }
func (r *row) pk() string    { return change.KeyJSON(r.ch.RowKey()) }

// Conflict kinds.
const (
	kindUpdateUpdate = "update_update" // local row is newer than a remote insert/update
	kindDeleteUpdate = "delete_update" // remote update/insert is older than a local delete
	kindUpdateDelete = "update_delete" // remote delete is older than a local update
	kindUnique       = "unique_violation"
	kindOwner        = "owner_violation"
)

// applyChange runs one change inside a savepoint so a unique violation on
// one row is recorded as a conflict instead of blocking the whole queue.
func (a *Applier) applyChange(ctx context.Context, tx pgx.Tx, r *row) error {
	if owner := a.cfg.Owner(r.ch.Schema, r.ch.Table); owner != "" && owner != r.origin {
		return a.conflict(ctx, tx, r, kindOwner, "skipped",
			fmt.Sprintf("table is owned by %q but change came from %q", owner, r.origin))
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	switch r.ch.Op {
	case "I", "U":
		err = a.upsert(ctx, sp, tx, r)
	case "D":
		err = a.remove(ctx, sp, tx, r)
	default:
		err = fmt.Errorf("unknown op %q", r.ch.Op)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		sp.Rollback(ctx)
		return a.conflict(ctx, tx, r, kindUnique, "skipped", pgErr.Message+" ("+pgErr.ConstraintName+")")
	}
	if err != nil {
		sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// localNewer reports whether the row exists locally and whether its last
// commit is newer than the remote change. Rows written before
// track_commit_timestamp was enabled count as oldest.
func localNewer(ctx context.Context, tx pgx.Tx, r *row, keys []change.Column) (exists, newer bool, err error) {
	var a args
	ts := a.addText(r.at.Format(time.RFC3339Nano))
	sql := fmt.Sprintf(`SELECT coalesce(pg_xact_commit_timestamp(xmin), '-infinity') > %s::timestamptz FROM %s WHERE %s FOR UPDATE`,
		ts, r.table(), where(&a, keys))
	err = tx.QueryRow(ctx, sql, a.params()...).Scan(&newer)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, newer, err
}

// deletedAfter reports whether a tombstone newer than the remote change exists.
func deletedAfter(ctx context.Context, tx pgx.Tx, r *row) (bool, error) {
	var newer bool
	err := tx.QueryRow(ctx,
		`SELECT deleted_at > $4 FROM conduit.tombstones WHERE schema_name = $1 AND table_name = $2 AND pk = $3`,
		r.ch.Schema, r.ch.Table, r.pk(), r.at).Scan(&newer)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return newer, err
}

func (a *Applier) upsert(ctx context.Context, sp, tx pgx.Tx, r *row) error {
	keys := r.ch.RowKey()
	exists, newer, err := localNewer(ctx, sp, r, keys)
	if err != nil {
		return err
	}
	if exists && newer {
		return a.conflict(ctx, tx, r, kindUpdateUpdate, "kept_local", "local row was modified later")
	}
	if !exists {
		if gone, err := deletedAfter(ctx, sp, r); err != nil || gone {
			if err != nil {
				return err
			}
			return a.conflict(ctx, tx, r, kindDeleteUpdate, "kept_delete", "row was deleted later")
		}
		if r.ch.Op == "U" && hasUnchanged(r.ch.New) {
			return a.conflict(ctx, tx, r, kindDeleteUpdate, "skipped", "row missing and unchanged TOAST values unknown")
		}
		return insert(ctx, sp, r)
	}
	return update(ctx, sp, r, keys)
}

func (a *Applier) remove(ctx context.Context, sp, tx pgx.Tx, r *row) error {
	keys := r.ch.RowKey()
	if len(keys) == 0 {
		return fmt.Errorf("delete without key columns")
	}
	exists, newer, err := localNewer(ctx, sp, r, keys)
	if err != nil {
		return err
	}
	if exists && newer {
		return a.conflict(ctx, tx, r, kindUpdateDelete, "kept_local", "local row was modified after the remote delete")
	}
	if exists {
		var ar args
		if _, err := ar.exec(ctx, sp, fmt.Sprintf("DELETE FROM %s WHERE %s", r.table(), where(&ar, keys))); err != nil {
			return err
		}
	}
	return store.AddTombstone(ctx, sp, r.ch.Schema, r.ch.Table, r.pk(), r.at)
}

func (a *Applier) conflict(ctx context.Context, tx pgx.Tx, r *row, kind, resolution, detail string) error {
	payload, err := json.Marshal(r.ch)
	if err != nil {
		return err
	}
	a.log.Warn("conflict", "kind", kind, "resolution", resolution, "table", r.ch.Schema+"."+r.ch.Table, "pk", r.pk(), "origin", r.origin)
	_, err = tx.Exec(ctx,
		`INSERT INTO conduit.conflicts (origin, schema_name, table_name, pk, kind, resolution, remote_time, change, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.origin, r.ch.Schema, r.ch.Table, r.pk(), kind, resolution, r.at, payload, detail)
	return err
}

func hasUnchanged(cols []change.Column) bool {
	for _, c := range cols {
		if c.Unchanged {
			return true
		}
	}
	return false
}

// Values travel as text and are sent as untyped SQL literals (simple
// protocol), so Postgres casts them to each column's type exactly as it
// would for pgoutput's own text output.
type args struct{ vals []any }

func (a *args) add(c change.Column) string {
	if c.Value == nil {
		a.vals = append(a.vals, nil)
	} else {
		a.vals = append(a.vals, *c.Value)
	}
	return "$" + strconv.Itoa(len(a.vals))
}

func (a *args) addText(s string) string {
	a.vals = append(a.vals, s)
	return "$" + strconv.Itoa(len(a.vals))
}

func (a *args) params() []any { return append([]any{pgx.QueryExecModeSimpleProtocol}, a.vals...) }

func (a *args) exec(ctx context.Context, tx pgx.Tx, sql string) (int64, error) {
	tag, err := tx.Exec(ctx, sql, a.params()...)
	return tag.RowsAffected(), err
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func where(a *args, keys []change.Column) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = ident(k.Name) + " = " + a.add(k)
	}
	return strings.Join(parts, " AND ")
}

func insert(ctx context.Context, tx pgx.Tx, r *row) error {
	var a args
	var names, vals []string
	for _, c := range r.ch.New {
		if c.Unchanged {
			continue
		}
		names = append(names, ident(c.Name))
		vals = append(vals, a.add(c))
	}
	// OVERRIDING SYSTEM VALUE keeps the origin's id for GENERATED ALWAYS columns.
	_, err := a.exec(ctx, tx, fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE VALUES (%s)",
		r.table(), strings.Join(names, ", "), strings.Join(vals, ", ")))
	return err
}

func update(ctx context.Context, tx pgx.Tx, r *row, keys []change.Column) error {
	var a args
	var sets []string
	keyChanged := len(r.ch.Old) > 0
	for _, c := range r.ch.New {
		// Leave unchanged keys out of SET: identity ALWAYS columns reject it.
		if !c.Unchanged && (keyChanged || !c.Key) {
			sets = append(sets, ident(c.Name)+" = "+a.add(c))
		}
	}
	if len(sets) == 0 {
		return nil
	}
	_, err := a.exec(ctx, tx, fmt.Sprintf("UPDATE %s SET %s WHERE %s", r.table(), strings.Join(sets, ", "), where(&a, keys)))
	return err
}

// ApplyBaseline writes snapshot rows that share one commit timestamp. The
// transaction is stamped with that timestamp so later conflict checks on
// this node see the same row ages as on the source. truncate (first call
// only) empties the listed tables first.
func (a *Applier) ApplyBaseline(ctx context.Context, origin string, at time.Time, truncate []string, rows []change.Change) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensure(ctx, origin); err != nil {
		a.reset()
		return err
	}
	tx, err := a.begin(ctx, "0/0", at)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if len(truncate) > 0 {
		names := make([]string, len(truncate))
		for i, t := range truncate {
			schema, table, _ := strings.Cut(t, ".")
			names[i] = pgx.Identifier{schema, table}.Sanitize()
		}
		if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")); err != nil {
			return err
		}
	}
	for _, ch := range rows {
		r := &row{origin: origin, at: at, ch: ch}
		var ar args
		var names, vals, keys, sets []string
		for _, c := range ch.New {
			names = append(names, ident(c.Name))
			vals = append(vals, ar.add(c))
			if c.Key {
				keys = append(keys, ident(c.Name))
			} else {
				sets = append(sets, ident(c.Name)+" = EXCLUDED."+ident(c.Name))
			}
		}
		sql := fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE VALUES (%s)", r.table(), strings.Join(names, ", "), strings.Join(vals, ", "))
		if len(keys) > 0 {
			sql += " ON CONFLICT (" + strings.Join(keys, ", ") + ") DO "
			if len(sets) == 0 {
				sql += "NOTHING"
			} else {
				sql += "UPDATE SET " + strings.Join(sets, ", ")
			}
		}
		if _, err := ar.exec(ctx, tx, sql); err != nil {
			return fmt.Errorf("%s: %w", r.table(), err)
		}
	}
	return tx.Commit(ctx)
}

// FinishBaseline records, per origin, the last seq already contained in the
// snapshot so those changes are not applied a second time.
func (a *Applier) FinishBaseline(ctx context.Context, cursors map[string]int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for origin, seq := range cursors {
		if _, err := a.conn.Exec(ctx,
			`INSERT INTO conduit.inbox_state (origin, applied_seq, updated_at) VALUES ($1, $2, now())
			 ON CONFLICT (origin) DO UPDATE SET applied_seq = greatest(conduit.inbox_state.applied_seq, EXCLUDED.applied_seq), updated_at = now()`,
			origin, seq); err != nil {
			return err
		}
	}
	return nil
}
