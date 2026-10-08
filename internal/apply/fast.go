// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/MorphEdit/conduit/internal/change"
)

// The fast path applies a whole chunk in two round trips instead of four per
// row: one query locks the chunk's rows and reports each row's state, the
// next writes every decided change. The decisions are the same as in the
// row-by-row path. If anything in the write fails (a unique clash, a schema
// difference), the chunk is rolled back to a savepoint and the caller falls
// back to the careful row-by-row path, which records each such conflict.

const fastMin = 16 // smaller chunks are not worth it

// Row states reported by the first query.
const (
	stMissing      = 0 // no such row, no newer delete
	stDeletedLater = 1 // no such row, deleted after the remote change
	stLocalNewer   = 2 // row exists and changed here after the remote change
	stOlder        = 3 // row exists and is older: the remote change wins
)

func (a *Applier) applyFast(ctx context.Context, tx pgx.Tx, origin string, at time.Time, changes []change.Change) (bool, error) {
	if len(changes) < fastMin {
		return false, nil
	}
	rows := make([]*row, len(changes))
	seen := make(map[string]bool, len(changes))
	for i, ch := range changes {
		r := &row{origin: origin, at: at, ch: ch}
		k := r.table() + r.pk()
		// Several changes to one row, owner rules, keyless deletes: let the
		// row-by-row path handle them in order.
		if seen[k] || len(ch.RowKey()) == 0 || (ch.Op != "I" && ch.Op != "U" && ch.Op != "D") {
			return false, nil
		}
		if !a.Syncable(ch.Schema) {
			return false, nil
		}
		if owner := a.cfg.Owner(ch.Schema, ch.Table); owner != "" && owner != origin {
			return false, nil
		}
		seen[k] = true
		rows[i] = r
	}
	ts := lit(ptr(at.Format(time.RFC3339Nano))) + "::timestamptz"

	// 1. lock the rows that exist, then read every row's state
	var q strings.Builder
	locks := map[string][]string{}
	var order []string
	for _, r := range rows {
		a := &args{literal: true}
		t := r.table()
		if _, ok := locks[t]; !ok {
			order = append(order, t)
		}
		locks[t] = append(locks[t], "("+where(a, r.ch.RowKey())+")")
	}
	for _, t := range order {
		fmt.Fprintf(&q, "SELECT 1 FROM %s WHERE %s FOR UPDATE;\n", t, strings.Join(locks[t], " OR "))
	}
	for i, r := range rows {
		a := &args{literal: true}
		cond := where(a, r.ch.RowKey())
		if i > 0 {
			q.WriteString(" UNION ALL\n")
		}
		fmt.Fprintf(&q, `SELECT %d, CASE
			WHEN EXISTS (SELECT 1 FROM %[2]s WHERE %[3]s) THEN
				CASE WHEN (SELECT coalesce(pg_xact_commit_timestamp(xmin), '-infinity') > %[4]s FROM %[2]s WHERE %[3]s) THEN %[5]d ELSE %[6]d END
			WHEN EXISTS (SELECT 1 FROM conduit.tombstones WHERE schema_name = %[7]s AND table_name = %[8]s AND pk = %[9]s AND deleted_at > %[4]s) THEN %[10]d
			ELSE %[11]d END`,
			i, r.table(), cond, ts, stLocalNewer, stOlder,
			lit(&r.ch.Schema), lit(&r.ch.Table), lit(ptr(r.pk())), stDeletedLater, stMissing)
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	results, err := tx.Conn().PgConn().Exec(ctx, q.String()).ReadAll()
	if err != nil || len(results) == 0 {
		sp.Rollback(ctx)
		return false, nil
	}
	states := make([]int, len(rows))
	for _, rec := range results[len(results)-1].Rows {
		i, _ := strconv.Atoi(string(rec[0]))
		s, _ := strconv.Atoi(string(rec[1]))
		if i >= 0 && i < len(states) {
			states[i] = s
		}
	}

	// 2. write everything that was decided
	var w strings.Builder
	conflicts := 0
	conflict := func(r *row, kind, resolution, detail string) {
		payload, _ := json.Marshal(r.ch)
		fmt.Fprintf(&w, `INSERT INTO conduit.conflicts (origin, schema_name, table_name, pk, kind, resolution, remote_time, change, detail)
			VALUES (%s, %s, %s, %s, %s, %s, %s, %s::jsonb, %s);`+"\n",
			lit(&r.origin), lit(&r.ch.Schema), lit(&r.ch.Table), lit(ptr(r.pk())), lit(&kind), lit(&resolution), ts,
			lit(ptr(string(payload))), lit(&detail))
		conflicts++
		a.log.Warn("conflict", "kind", kind, "resolution", resolution, "table", r.ch.Schema+"."+r.ch.Table, "pk", r.pk(), "origin", r.origin)
	}
	for i, r := range rows {
		keys := r.ch.RowKey()
		la := &args{literal: true}
		switch st := states[i]; {
		case r.ch.Op == "D" && st == stLocalNewer:
			conflict(r, kindUpdateDelete, "kept_local", "local row was modified after the remote delete")
		case r.ch.Op == "D":
			if st == stOlder {
				fmt.Fprintf(&w, "DELETE FROM %s WHERE %s;\n", r.table(), where(la, keys))
			}
			fmt.Fprintf(&w, `INSERT INTO conduit.tombstones (schema_name, table_name, pk, deleted_at) VALUES (%s, %s, %s, %s)
				ON CONFLICT (schema_name, table_name, pk) DO UPDATE SET deleted_at = greatest(conduit.tombstones.deleted_at, EXCLUDED.deleted_at);`+"\n",
				lit(&r.ch.Schema), lit(&r.ch.Table), lit(ptr(r.pk())), ts)
		case st == stLocalNewer:
			conflict(r, kindUpdateUpdate, "kept_local", "local row was modified later")
		case st == stDeletedLater:
			conflict(r, kindDeleteUpdate, "kept_delete", "row was deleted later")
		case st == stMissing && r.ch.Op == "U" && hasUnchanged(r.ch.New):
			conflict(r, kindDeleteUpdate, "skipped", "row missing and unchanged TOAST values unknown")
		case st == stMissing:
			w.WriteString(insertSQL(la, r) + ";\n")
		default: // stOlder
			if s := updateSQL(la, r, keys); s != "" {
				w.WriteString(s + ";\n")
			}
		}
	}
	if w.Len() > 0 {
		if _, err := tx.Conn().PgConn().Exec(ctx, w.String()).ReadAll(); err != nil {
			sp.Rollback(ctx)
			return false, nil // let the row-by-row path find and record the problem
		}
	}
	return true, sp.Commit(ctx)
}

func ptr(s string) *string { return &s }
