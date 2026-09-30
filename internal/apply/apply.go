// Package apply writes a peer's transactions into the local database.
package apply

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/conduit-sync/conduit/internal/change"
)

// Applier owns one dedicated connection. The connection is tagged with a
// replication origin per peer, so the local capture ("origin 'none'") never
// sends applied rows back to where they came from.
type Applier struct {
	dsn string
	log *slog.Logger

	mu     sync.Mutex
	conn   *pgx.Conn
	origin string
}

func New(dsn string, log *slog.Logger) *Applier {
	return &Applier{dsn: dsn, log: log.With("component", "apply")}
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
		conn, err := pgx.Connect(ctx, a.dsn)
		if err != nil {
			return err
		}
		// Like Postgres' own apply workers: don't fire user triggers (e.g.
		// audit logs) a second time, and don't enforce FK order per row.
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

func (a *Applier) applyTx(ctx context.Context, origin string, tx change.Tx) error {
	dbtx, err := a.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer dbtx.Rollback(ctx)

	for i, ch := range tx.Changes {
		if err := a.applyChange(ctx, dbtx, ch); err != nil {
			return fmt.Errorf("change %d (%s %s.%s): %w", i, ch.Op, ch.Schema, ch.Table, err)
		}
	}
	if _, err := dbtx.Exec(ctx,
		`INSERT INTO conduit.inbox_state (origin, applied_seq, updated_at) VALUES ($1, $2, now())
		 ON CONFLICT (origin) DO UPDATE SET applied_seq = EXCLUDED.applied_seq, updated_at = now()`,
		origin, tx.Seq); err != nil {
		return err
	}
	return dbtx.Commit(ctx)
}

func (a *Applier) applyChange(ctx context.Context, tx pgx.Tx, ch change.Change) error {
	switch ch.Op {
	case "I":
		return insert(ctx, tx, ch)
	case "U":
		return a.update(ctx, tx, ch)
	case "D":
		return remove(ctx, tx, ch)
	}
	return fmt.Errorf("unknown op %q", ch.Op)
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

func (a *args) exec(ctx context.Context, tx pgx.Tx, sql string) (int64, error) {
	tag, err := tx.Exec(ctx, sql, append([]any{pgx.QueryExecModeSimpleProtocol}, a.vals...)...)
	return tag.RowsAffected(), err
}

func table(ch change.Change) string { return pgx.Identifier{ch.Schema, ch.Table}.Sanitize() }
func ident(s string) string         { return pgx.Identifier{s}.Sanitize() }

func where(a *args, keys []change.Column) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = ident(k.Name) + " = " + a.add(k)
	}
	return strings.Join(parts, " AND ")
}

// insert upserts on the primary key so a row that already exists converges
// to the sender's version instead of failing the whole queue.
func insert(ctx context.Context, tx pgx.Tx, ch change.Change) error {
	var a args
	var names, vals, keys, sets []string
	for _, c := range ch.New {
		if c.Unchanged {
			continue
		}
		names = append(names, ident(c.Name))
		vals = append(vals, a.add(c))
		if c.Key {
			keys = append(keys, ident(c.Name))
		} else {
			sets = append(sets, ident(c.Name)+" = EXCLUDED."+ident(c.Name))
		}
	}
	sql := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table(ch), strings.Join(names, ", "), strings.Join(vals, ", "))
	if len(keys) > 0 {
		sql += " ON CONFLICT (" + strings.Join(keys, ", ") + ")"
		if len(sets) == 0 {
			sql += " DO NOTHING"
		} else {
			sql += " DO UPDATE SET " + strings.Join(sets, ", ")
		}
	}
	_, err := a.exec(ctx, tx, sql)
	return err
}

func (ap *Applier) update(ctx context.Context, tx pgx.Tx, ch change.Change) error {
	var a args
	var sets []string
	unchanged := false
	for _, c := range ch.New {
		if c.Unchanged {
			unchanged = true
			continue
		}
		sets = append(sets, ident(c.Name)+" = "+a.add(c))
	}
	keys := ch.Old
	if len(keys) == 0 {
		keys = change.KeyColumns(ch.New)
	}
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s", table(ch), strings.Join(sets, ", "), where(&a, keys))
	n, err := a.exec(ctx, tx, sql)
	if err != nil || n > 0 {
		return err
	}
	// Row is missing here. Recreate it if we have every column.
	if unchanged {
		ap.log.Warn("update for missing row skipped: unchanged TOAST columns unknown", "table", table(ch))
		return nil
	}
	return insert(ctx, tx, ch)
}

func remove(ctx context.Context, tx pgx.Tx, ch change.Change) error {
	if len(ch.Old) == 0 {
		return fmt.Errorf("delete without key columns")
	}
	var a args
	_, err := a.exec(ctx, tx, fmt.Sprintf("DELETE FROM %s WHERE %s", table(ch), where(&a, ch.Old)))
	return err
}
