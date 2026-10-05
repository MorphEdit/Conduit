// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package change defines the wire format Conduit nodes exchange.
package change

import (
	"encoding/json"
	"time"
)

// Column is one column value in text form, exactly as pgoutput delivers it.
type Column struct {
	Name string `json:"n"`
	// Key marks a replica-identity (primary key) column.
	Key bool `json:"k,omitempty"`
	// Value is nil for SQL NULL.
	Value *string `json:"v"`
	// Unchanged marks a TOASTed value that was not modified by an UPDATE and
	// therefore was not sent; it must be left as-is on the receiving side.
	Unchanged bool `json:"u,omitempty"`
}

// Change is a single row-level operation.
type Change struct {
	Op     string   `json:"op"` // "I" insert, "U" update, "D" delete
	Schema string   `json:"s"`
	Table  string   `json:"t"`
	New    []Column `json:"new,omitempty"`
	// Old identifies the row before the change (key columns only).
	Old []Column `json:"old,omitempty"`
}

// Tx is one committed source transaction, stored as one outbox row.
//
// A big source transaction is split into several Tx parts with the same LSN
// and increasing Part numbers; every part but the last has Partial set. The
// receiver keeps partial parts on disk and applies them all, in one database
// transaction, when the last part arrives.
type Tx struct {
	Seq        int64     `json:"seq"`
	LSN        string    `json:"lsn"`
	CommitTime time.Time `json:"commit_time"`
	Part       int       `json:"part,omitempty"`
	Partial    bool      `json:"partial,omitempty"`
	Changes    []Change  `json:"changes"`
}

// Size estimates how many bytes a change takes once encoded.
func (c Change) Size() int {
	n := 48 + len(c.Schema) + len(c.Table)
	for _, cols := range [][]Column{c.New, c.Old} {
		for _, col := range cols {
			n += 16 + len(col.Name)
			if col.Value != nil {
				n += len(*col.Value)
			}
		}
	}
	return n
}

// Batch is what a sender POSTs to a peer.
type Batch struct {
	Origin string `json:"origin"`
	Txs    []Tx   `json:"txs"`
}

// Ack is the peer's reply: the highest seq from Origin it has durably applied.
type Ack struct {
	Applied int64  `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// KeyColumns returns the key columns, or all columns if none are flagged
// (REPLICA IDENTITY FULL tables).
func KeyColumns(cols []Column) []Column {
	var keys []Column
	for _, c := range cols {
		if c.Key {
			keys = append(keys, c)
		}
	}
	if len(keys) == 0 {
		return cols
	}
	return keys
}

// KeyJSON renders key columns as a canonical JSON object ({"col":"val"},
// keys sorted) used to identify a row in tombstones and conflict records.
func KeyJSON(keys []Column) string {
	m := make(map[string]*string, len(keys))
	for _, k := range keys {
		m[k.Name] = k.Value
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// RowKey returns the columns that identify the row a change targets.
func (c Change) RowKey() []Column {
	if len(c.Old) > 0 {
		return c.Old
	}
	return KeyColumns(c.New)
}
