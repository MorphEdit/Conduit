// Package snapshot copies a peer's full table contents to a new node so it
// can join sync without replaying history.
//
// Wire format: newline-delimited JSON. One "header", then "row" lines
// grouped by table and ordered by each row's commit timestamp, then "end".
package snapshot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/apply"
	"github.com/conduit-sync/conduit/internal/change"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/peer"
)

type line struct {
	Type string `json:"type"` // header | row | end

	// header
	Origin string           `json:"origin,omitempty"`
	Seq    int64            `json:"seq,omitempty"`
	Inbox  map[string]int64 `json:"inbox,omitempty"`
	Tables []string         `json:"tables,omitempty"`

	// row
	At     *time.Time     `json:"at,omitempty"`
	Change *change.Change `json:"change,omitempty"`

	// end
	Rows int64 `json:"rows,omitempty"`
}

// Serve streams a consistent snapshot of every published table.
//
// The outbox position is read BEFORE the repeatable-read snapshot starts, so
// the receiver may see a few changes again after joining; those replays are
// harmless because they are applied in order and compared by timestamp.
// Cursors for other origins are read INSIDE the snapshot, so they match the
// data exactly.
func Serve(ctx context.Context, w http.ResponseWriter, pool *pgxpool.Pool, cfg *config.Config) error {
	var seq int64
	if err := pool.QueryRow(ctx,
		`SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM conduit.outbox_seq_seq`).Scan(&seq); err != nil {
		return err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	inbox := map[string]int64{}
	rows, err := tx.Query(ctx, `SELECT origin, applied_seq FROM conduit.inbox_state`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o string
		var s int64
		if err := rows.Scan(&o, &s); err != nil {
			return err
		}
		inbox[o] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = tx.Query(ctx,
		`SELECT schemaname || '.' || tablename FROM pg_publication_tables WHERE pubname = $1 ORDER BY 1`,
		cfg.Capture.Publication)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	bw := bufio.NewWriterSize(w, 64<<10)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(line{Type: "header", Origin: cfg.NodeID, Seq: seq, Inbox: inbox, Tables: tables}); err != nil {
		return err
	}

	var total int64
	for _, t := range tables {
		n, err := dumpTable(ctx, tx, enc, t)
		if err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		total += n
	}
	if err := enc.Encode(line{Type: "end", Rows: total}); err != nil {
		return err
	}
	return bw.Flush()
}

type column struct {
	name string
	key  bool
}

func dumpTable(ctx context.Context, tx pgx.Tx, enc *json.Encoder, qualified string) (int64, error) {
	schema, table, _ := strings.Cut(qualified, ".")
	ident := pgx.Identifier{schema, table}.Sanitize()

	rows, err := tx.Query(ctx, `
		SELECT a.attname, coalesce(a.attnum = ANY(i.indkey), false)
		FROM pg_attribute a
		LEFT JOIN pg_index i ON i.indrelid = a.attrelid AND i.indisprimary
		WHERE a.attrelid = $1::regclass AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		ORDER BY a.attnum`, ident)
	if err != nil {
		return 0, err
	}
	cols, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (column, error) {
		var c column
		return c, r.Scan(&c.name, &c.key)
	})
	if err != nil {
		return 0, err
	}

	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = pgx.Identifier{c.name}.Sanitize() + "::text"
	}
	rows, err = tx.Query(ctx, fmt.Sprintf(
		`SELECT coalesce(pg_xact_commit_timestamp(xmin), 'epoch') AS ts, %s FROM %s ORDER BY ts`,
		strings.Join(exprs, ", "), ident))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var n int64
	vals := make([]*string, len(cols))
	dest := make([]any, len(cols)+1)
	var at time.Time
	dest[0] = &at
	for i := range vals {
		dest[i+1] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return n, err
		}
		ch := change.Change{Op: "I", Schema: schema, Table: table, New: make([]change.Column, len(cols))}
		for i, c := range cols {
			ch.New[i] = change.Column{Name: c.name, Key: c.key, Value: vals[i]}
		}
		ts := at
		if err := enc.Encode(line{Type: "row", At: &ts, Change: &ch}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// Pull loads a snapshot from the peer at peerURL into the local database.
// Nothing else may be applying to this database while it runs.
func Pull(ctx context.Context, cfg *config.Config, peerID string, target peer.Target, creds peer.Credentials, truncate bool, ap *apply.Applier, log *slog.Logger) error {
	resp, err := peer.Do(ctx, target, creds, http.MethodGet, "/v1/snapshot", nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer returned %s", resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), 256<<20)
	next := func() (line, error) {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return line{}, err
			}
			return line{}, errors.New("snapshot stream ended early")
		}
		var l line
		return l, json.Unmarshal(sc.Bytes(), &l)
	}

	hdr, err := next()
	if err != nil {
		return err
	}
	if hdr.Type != "header" || hdr.Origin != peerID {
		return fmt.Errorf("unexpected snapshot header from %q", hdr.Origin)
	}
	log.Info("snapshot started", "from", peerID, "tables", len(hdr.Tables), "outbox_seq", hdr.Seq)

	var pendingTruncate []string
	if truncate {
		pendingTruncate = hdr.Tables
	}
	var (
		group   []change.Change
		groupAt time.Time
		applied int64
	)
	flush := func() error {
		if err := ap.ApplyBaseline(ctx, peerID, groupAt, pendingTruncate, group); err != nil {
			return err
		}
		applied += int64(len(group))
		pendingTruncate, group = nil, group[:0]
		return nil
	}

	for {
		l, err := next()
		if err != nil {
			return err
		}
		if l.Type == "end" {
			if len(group) > 0 || pendingTruncate != nil || applied == 0 {
				if err := flush(); err != nil {
					return err
				}
			}
			if applied != l.Rows {
				return fmt.Errorf("applied %d rows but peer sent %d", applied, l.Rows)
			}
			break
		}
		if l.Type != "row" || l.Change == nil || l.At == nil {
			return fmt.Errorf("bad snapshot line %q", l.Type)
		}
		if len(group) > 0 && (!l.At.Equal(groupAt) || len(group) >= 500) {
			if err := flush(); err != nil {
				return err
			}
		}
		groupAt = *l.At
		group = append(group, *l.Change)
	}

	cursors := map[string]int64{peerID: hdr.Seq}
	for origin, seq := range hdr.Inbox {
		if origin != cfg.NodeID {
			cursors[origin] = seq
		}
	}
	if err := ap.FinishBaseline(ctx, cursors); err != nil {
		return err
	}
	log.Info("snapshot applied", "rows", applied, "cursors", cursors)
	return nil
}
