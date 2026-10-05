// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package capture reads row changes from Postgres logical replication
// (pgoutput) and writes each committed transaction to the outbox.
package capture

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/MorphEdit/conduit/internal/change"
	"github.com/MorphEdit/conduit/internal/config"
	"github.com/MorphEdit/conduit/internal/notify"
	"github.com/MorphEdit/conduit/internal/store"
)

const statusInterval = 10 * time.Second

// A transaction is written to the outbox in parts of at most this many
// changes / bytes, so memory stays small however big the transaction is.
const (
	partChanges = 2000
	partBytes   = 2 << 20
)

type Status struct {
	Enabled      bool       `json:"enabled"`
	Connected    bool       `json:"connected"`
	ConfirmedLSN string     `json:"confirmed_lsn,omitempty"`
	LastCommit   *time.Time `json:"last_commit,omitempty"`
	Captured     int64      `json:"captured_txs"`
	LastError    string     `json:"last_error,omitempty"`
}

type Capture struct {
	cfg   config.CaptureConfig
	dsn   string
	store *store.Store
	bus   *notify.Bus
	log   *slog.Logger

	relations map[uint32]*pglogrepl.RelationMessage

	mu sync.Mutex
	st Status
}

func New(cfg *config.Config, st *store.Store, bus *notify.Bus, log *slog.Logger) *Capture {
	return &Capture{
		cfg:   cfg.Capture,
		dsn:   cfg.Database,
		store: st,
		bus:   bus,
		log:   log.With("component", "capture"),
		st:    Status{Enabled: true},
	}
}

func (c *Capture) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st
}

// Setup checks wal_level and creates the publication and slot if missing.
func (c *Capture) Setup(ctx context.Context) error {
	pool := c.store.Pool()

	var walLevel string
	if err := pool.QueryRow(ctx, `SHOW wal_level`).Scan(&walLevel); err != nil {
		return err
	}
	if walLevel != "logical" {
		return fmt.Errorf("wal_level is %q; set wal_level=logical and restart Postgres", walLevel)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = $1)`, c.cfg.Publication).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		schemas := make([]string, len(c.cfg.Schemas))
		for i, s := range c.cfg.Schemas {
			schemas[i] = pgx.Identifier{s}.Sanitize()
		}
		q := fmt.Sprintf(`CREATE PUBLICATION %s FOR TABLES IN SCHEMA %s`,
			pgx.Identifier{c.cfg.Publication}.Sanitize(), strings.Join(schemas, ", "))
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("create publication: %w", err)
		}
		c.log.Info("created publication", "name", c.cfg.Publication, "schemas", c.cfg.Schemas)
	}

	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, c.cfg.Slot).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := pool.Exec(ctx, `SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, c.cfg.Slot); err != nil {
			return fmt.Errorf("create replication slot: %w", err)
		}
		c.log.Info("created replication slot", "name", c.cfg.Slot)
	}
	return nil
}

// Run streams changes until ctx is cancelled, reconnecting with backoff.
func (c *Capture) Run(ctx context.Context) {
	backoff := time.Second
	for {
		started := time.Now()
		err := c.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		c.setError(err)
		c.log.Warn("replication stream ended; reconnecting", "err", err, "in", backoff)
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (c *Capture) stream(ctx context.Context) error {
	pcfg, err := pgconn.ParseConfig(c.dsn)
	if err != nil {
		return err
	}
	pcfg.RuntimeParams["replication"] = "database"
	conn, err := pgconn.ConnectConfig(ctx, pcfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	args := []string{
		"proto_version '1'",
		fmt.Sprintf("publication_names '%s'", c.cfg.Publication),
		// Skip changes that Conduit itself applied from a peer (they carry a
		// replication origin). This is what stops two-way sync from looping.
		"origin 'none'",
	}
	// startLSN 0 resumes from the slot's confirmed position.
	if err := pglogrepl.StartReplication(ctx, conn, c.cfg.Slot, 0,
		pglogrepl.StartReplicationOptions{PluginArgs: args}); err != nil {
		return fmt.Errorf("start replication: %w", err)
	}
	c.relations = map[uint32]*pglogrepl.RelationMessage{}
	c.setConnected(true)
	defer c.setConnected(false)
	c.log.Info("streaming changes", "slot", c.cfg.Slot)

	var (
		confirmed    pglogrepl.LSN
		inTx         bool
		pending      []change.Change
		pendingBytes int
		txLSN        string
		txTime       time.Time
		part         int
		nextStatus   = time.Now().Add(statusInterval)
	)
	// flush writes the changes collected so far as one part of the current
	// transaction. The (lsn, part) key makes a replay after a crash harmless.
	flush := func(final bool) error {
		if err := c.store.AppendOutbox(ctx, txLSN, part, final, txTime, pending); err != nil {
			return fmt.Errorf("append outbox: %w", err)
		}
		part++
		pending, pendingBytes = nil, 0
		c.bus.Publish()
		return nil
	}
	add := func(ch change.Change) error {
		pending = append(pending, ch)
		pendingBytes += ch.Size()
		if len(pending) >= partChanges || pendingBytes >= partBytes {
			return flush(false)
		}
		return nil
	}
	for {
		if !time.Now().Before(nextStatus) {
			if confirmed > 0 {
				if err := pglogrepl.SendStandbyStatusUpdate(ctx, conn,
					pglogrepl.StandbyStatusUpdate{WALWritePosition: confirmed}); err != nil {
					return fmt.Errorf("status update: %w", err)
				}
				c.setConfirmed(confirmed)
			}
			nextStatus = time.Now().Add(statusInterval)
		}

		rctx, cancel := context.WithDeadline(ctx, nextStatus)
		raw, err := conn.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if pgconn.Timeout(err) {
				continue
			}
			return err
		}

		var data []byte
		switch m := raw.(type) {
		case *pgproto3.ErrorResponse:
			return fmt.Errorf("server error: %s", m.Message)
		case *pgproto3.CopyData:
			data = m.Data
		default:
			continue
		}

		switch data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pkm, err := pglogrepl.ParsePrimaryKeepaliveMessage(data[1:])
			if err != nil {
				return err
			}
			// Between transactions everything received so far is already in
			// the outbox, so the server may recycle WAL up to here.
			if !inTx && pkm.ServerWALEnd > confirmed {
				confirmed = pkm.ServerWALEnd
			}
			if pkm.ReplyRequested {
				nextStatus = time.Time{}
			}

		case pglogrepl.XLogDataByteID:
			xld, err := pglogrepl.ParseXLogData(data[1:])
			if err != nil {
				return err
			}
			msg, err := pglogrepl.Parse(xld.WALData)
			if err != nil {
				return fmt.Errorf("decode pgoutput: %w", err)
			}
			switch m := msg.(type) {
			case *pglogrepl.RelationMessage:
				c.relations[m.RelationID] = m
			case *pglogrepl.BeginMessage:
				inTx, pending, pendingBytes, part = true, nil, 0, 0
				txLSN, txTime = m.FinalLSN.String(), m.CommitTime
			case *pglogrepl.InsertMessage:
				ch, err := c.row("I", m.RelationID, m.Tuple, nil)
				if err != nil {
					return err
				}
				if err := add(ch); err != nil {
					return err
				}
			case *pglogrepl.UpdateMessage:
				ch, err := c.row("U", m.RelationID, m.NewTuple, m.OldTuple)
				if err != nil {
					return err
				}
				if err := add(ch); err != nil {
					return err
				}
			case *pglogrepl.DeleteMessage:
				ch, err := c.row("D", m.RelationID, nil, m.OldTuple)
				if err != nil {
					return err
				}
				if err := add(ch); err != nil {
					return err
				}
			case *pglogrepl.TruncateMessage:
				c.log.Warn("TRUNCATE is not replicated; run it on every node yourself")
			case *pglogrepl.CommitMessage:
				if len(pending) > 0 || part > 0 {
					// Durably queue before confirming the slot position:
					// a crash here replays the transaction, never loses it.
					if err := flush(true); err != nil {
						return err
					}
					c.committed(m.CommitTime)
				}
				confirmed = m.TransactionEndLSN
				inTx, pending, part = false, nil, 0
				nextStatus = time.Time{}
			}
		}
	}
}

func (c *Capture) row(op string, relID uint32, newTuple, oldTuple *pglogrepl.TupleData) (change.Change, error) {
	rel, ok := c.relations[relID]
	if !ok {
		return change.Change{}, fmt.Errorf("unknown relation id %d", relID)
	}
	ch := change.Change{Op: op, Schema: rel.Namespace, Table: rel.RelationName}
	if newTuple != nil {
		ch.New = columns(rel, newTuple)
	}
	if oldTuple != nil {
		ch.Old = change.KeyColumns(columns(rel, oldTuple))
	}
	return ch, nil
}

func columns(rel *pglogrepl.RelationMessage, t *pglogrepl.TupleData) []change.Column {
	cols := make([]change.Column, 0, len(t.Columns))
	for i, tc := range t.Columns {
		rc := rel.Columns[i]
		col := change.Column{Name: rc.Name, Key: rc.Flags == 1}
		switch tc.DataType {
		case pglogrepl.TupleDataTypeNull:
		case pglogrepl.TupleDataTypeToast:
			col.Unchanged = true
		case pglogrepl.TupleDataTypeText:
			v := string(tc.Data)
			col.Value = &v
		}
		cols = append(cols, col)
	}
	return cols
}

func (c *Capture) setConnected(v bool) {
	c.mu.Lock()
	c.st.Connected = v
	if v {
		c.st.LastError = ""
	}
	c.mu.Unlock()
}

func (c *Capture) setConfirmed(lsn pglogrepl.LSN) {
	c.mu.Lock()
	c.st.ConfirmedLSN = lsn.String()
	c.mu.Unlock()
}

func (c *Capture) committed(t time.Time) {
	c.mu.Lock()
	c.st.Captured++
	c.st.LastCommit = &t
	c.mu.Unlock()
}

func (c *Capture) setError(err error) {
	c.mu.Lock()
	c.st.LastError = err.Error()
	c.mu.Unlock()
}
