// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

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

	"github.com/MorphEdit/conduit/internal/config"
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

// alignSQL installs conduit.align_sequences(): every id sequence in the
// synced schemas produces only values v with v % step == offset % step,
// starting above anything already in the table (rows replicated from peers
// do not advance local sequences). An event trigger runs it right after any
// CREATE/ALTER TABLE or sequence, inside the same transaction, so a table
// created while Conduit runs can never hand out another site's ids. It only
// warns on failure: it must never break the user's DDL.
var alignSQL = []string{
	`ALTER TABLE conduit.node ADD COLUMN IF NOT EXISTS schemas TEXT[] NOT NULL DEFAULT '{public}'`,
	// SECURITY DEFINER: it runs with Conduit's rights even when an app's
	// ordinary database user creates the table (that user cannot alter
	// sequences it does not own, nor read the conduit schema).
	`CREATE OR REPLACE FUNCTION conduit.align_sequences() RETURNS integer LANGUAGE plpgsql
	SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $fn$
	DECLARE
		r record; v_step bigint; v_off bigint; v_schemas text[];
		v_max bigint; v_last bigint; v_called boolean; v_inc bigint; v_next bigint; v_floor bigint; v_target bigint;
		v_done integer := 0;
	BEGIN
		SELECT id_step, id_offset, schemas INTO v_step, v_off, v_schemas FROM conduit.node WHERE ready;
		IF NOT FOUND OR v_step IS NULL THEN RETURN 0; END IF;
		PERFORM set_config('conduit.aligning', 'on', true);
		FOR r IN
			SELECT n.nspname AS sch, c.relname AS tbl, a.attname AS col, a.attidentity <> '' AS ident,
			       pg_get_serial_sequence(format('%I.%I', n.nspname, c.relname), a.attname) AS seq
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
			WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY (v_schemas)
			  AND pg_get_serial_sequence(format('%I.%I', n.nspname, c.relname), a.attname) IS NOT NULL
		LOOP
			EXECUTE format('SELECT coalesce(max(%I), 0)::bigint FROM %I.%I', r.col, r.sch, r.tbl) INTO v_max;
			EXECUTE format('SELECT last_value, is_called FROM %s', r.seq) INTO v_last, v_called;
			SELECT increment_by INTO v_inc FROM pg_sequences WHERE format('%I.%I', schemaname, sequencename) = r.seq;
			v_next := CASE WHEN v_called THEN v_last + v_inc ELSE v_last END;
			CONTINUE WHEN v_inc = v_step AND mod(v_next, v_step) = mod(v_off, v_step) AND v_next > v_max;
			v_floor := greatest(v_max, v_next - 1);
			v_target := v_floor + 1 + mod(mod(v_off - (v_floor + 1), v_step) + v_step, v_step);
			IF r.ident THEN
				EXECUTE format('ALTER TABLE %I.%I ALTER COLUMN %I SET INCREMENT BY %s', r.sch, r.tbl, r.col, v_step);
			ELSE
				EXECUTE format('ALTER SEQUENCE %s INCREMENT BY %s', r.seq, v_step);
			END IF;
			PERFORM setval(r.seq::regclass, v_target, false);
			v_done := v_done + 1;
		END LOOP;
		PERFORM set_config('conduit.aligning', '', true);
		RETURN v_done;
	END $fn$`,
	`CREATE OR REPLACE FUNCTION conduit.align_on_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $fn$
	BEGIN
		IF coalesce(current_setting('conduit.aligning', true), '') = 'on' THEN RETURN; END IF;
		PERFORM conduit.align_sequences();
	EXCEPTION WHEN OTHERS THEN
		RAISE WARNING 'conduit: could not align id sequences: %', SQLERRM;
	END $fn$`,
	// Every user must be able to reach these two functions (the event
	// trigger and owner guards run as the user doing the DDL / write);
	// Conduit's tables stay private.
	`GRANT USAGE ON SCHEMA conduit TO PUBLIC`,
	`GRANT EXECUTE ON FUNCTION conduit.align_sequences(), conduit.align_on_ddl(), conduit.owner_guard() TO PUBLIC`,
	`DO $do$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = 'conduit_align_ids') THEN
			CREATE EVENT TRIGGER conduit_align_ids ON ddl_command_end
				WHEN TAG IN ('CREATE TABLE', 'CREATE TABLE AS', 'SELECT INTO', 'ALTER TABLE', 'CREATE SEQUENCE', 'ALTER SEQUENCE')
				EXECUTE FUNCTION conduit.align_on_ddl();
		END IF;
	END $do$`,
}

// alignSequences installs the SQL side and runs it once now.
func alignSequences(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, log *slog.Logger) error {
	for _, q := range alignSQL {
		if _, err := pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE conduit.node SET schemas = $1`, cfg.Capture.Schemas); err != nil {
		return err
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT conduit.align_sequences()`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		log.Info("aligned id sequences", "count", n, "offset", cfg.Sequences.Offset, "step", cfg.Sequences.Step)
	}
	return nil
}

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
	for name, p := range cfg.TablesCopy() {
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
