// Package policy prepares a node's tables for multi-writer sync: it moves
// id sequences into the node's own residue class and installs guard
// triggers on tables owned by another node.
package policy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/config"
)

const guardTrigger = "conduit_owner_guard"

// Setup is idempotent and runs at startup and after a snapshot.
func Setup(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) error {
	log = log.With("component", "policy")
	if cfg.Sequences != nil {
		if err := alignSequences(ctx, pool, cfg, log); err != nil {
			return fmt.Errorf("align sequences: %w", err)
		}
	}
	if err := installGuards(ctx, pool, cfg, log); err != nil {
		return fmt.Errorf("owner guards: %w", err)
	}
	return nil
}

type seqCol struct {
	schema, table, column, seq string
	identity                   bool
}

// alignSequences makes every id sequence produce only values v with
// v % step == offset % step, starting above anything already in the table
// (rows replicated from peers do not advance local sequences).
func alignSequences(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) error {
	rows, err := pool.Query(ctx, `
		SELECT n.nspname, c.relname, a.attname,
		       pg_get_serial_sequence(format('%I.%I', n.nspname, c.relname), a.attname),
		       a.attidentity <> ''
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1)
		  AND pg_get_serial_sequence(format('%I.%I', n.nspname, c.relname), a.attname) IS NOT NULL
		ORDER BY 1, 2, 3`, cfg.Capture.Schemas)
	if err != nil {
		return err
	}
	cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seqCol, error) {
		var s seqCol
		return s, r.Scan(&s.schema, &s.table, &s.column, &s.seq, &s.identity)
	})
	if err != nil {
		return err
	}

	step, offset := cfg.Sequences.Step, cfg.Sequences.Offset
	for _, sc := range cols {
		tbl := pgx.Identifier{sc.schema, sc.table}.Sanitize()
		col := pgx.Identifier{sc.column}.Sanitize()

		var maxID, last, inc int64
		var called bool
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT coalesce(max(%s), 0)::bigint FROM %s`, col, tbl)).Scan(&maxID); err != nil {
			return err
		}
		if err := pool.QueryRow(ctx, fmt.Sprintf(`SELECT last_value, is_called FROM %s`, sc.seq)).Scan(&last, &called); err != nil {
			return err
		}
		if err := pool.QueryRow(ctx,
			`SELECT increment_by FROM pg_sequences WHERE format('%I.%I', schemaname, sequencename) = $1`, sc.seq).Scan(&inc); err != nil {
			return err
		}

		next := last
		if called {
			next = last + inc
		}
		if inc == step && mod(next, step) == mod(offset, step) && next > maxID {
			continue
		}

		floor := max(maxID, next-1)
		target := floor + 1 + mod(offset-(floor+1), step)
		if sc.identity {
			_, err = pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET INCREMENT BY %d`, tbl, col, step))
		} else {
			_, err = pool.Exec(ctx, fmt.Sprintf(`ALTER SEQUENCE %s INCREMENT BY %d`, sc.seq, step))
		}
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `SELECT setval($1::regclass, $2, false)`, sc.seq, target); err != nil {
			return err
		}
		log.Info("aligned id sequence", "table", sc.schema+"."+sc.table, "column", sc.column, "next", target, "step", step)
	}
	return nil
}

func mod(a, m int64) int64 { return ((a % m) + m) % m }

// installGuards puts a write-blocking trigger on every table owned by another
// node and removes stale guards (ownership changed or table now local).
func installGuards(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) error {
	rows, err := pool.Query(ctx, `
		SELECT n.nspname || '.' || c.relname
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE t.tgname = $1`, guardTrigger)
	if err != nil {
		return err
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	want := map[string]string{}
	for name, p := range cfg.Tables {
		if p.Owner != "" && p.Owner != cfg.NodeID {
			want[name] = p.Owner
		}
	}
	for _, name := range existing {
		if _, ok := want[name]; !ok {
			if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER %s ON %s`, guardTrigger, qualified(name))); err != nil {
				return err
			}
			log.Info("removed owner guard", "table", name)
		}
	}
	for name, owner := range want {
		q := fmt.Sprintf(`CREATE OR REPLACE TRIGGER %s BEFORE INSERT OR UPDATE OR DELETE OR TRUNCATE ON %s
			FOR EACH STATEMENT EXECUTE FUNCTION conduit.owner_guard(%s)`,
			guardTrigger, qualified(name), quoteLiteral(owner))
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		log.Info("table is read-only here", "table", name, "owner", owner)
	}
	return nil
}

func qualified(name string) string {
	schema, table, _ := strings.Cut(name, ".")
	return pgx.Identifier{schema, table}.Sanitize()
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
