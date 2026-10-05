// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package change

import (
	"encoding/json"
	"testing"
)

func str(s string) *string { return &s }

func TestSizeIsAnUpperEstimate(t *testing.T) {
	c := Change{Op: "U", Schema: "public", Table: "customers",
		New: []Column{{Name: "id", Key: true, Value: str("42")}, {Name: "name", Value: str("บริษัท ก จำกัด")}, {Name: "note"}},
		Old: []Column{{Name: "id", Key: true, Value: str("41")}}}
	raw, _ := json.Marshal(c)
	if c.Size() < len(raw)/2 || c.Size() > len(raw)*4 {
		t.Fatalf("Size() = %d is far from the encoded size %d", c.Size(), len(raw))
	}
}

func TestKeyJSONIsCanonical(t *testing.T) {
	a := KeyJSON([]Column{{Name: "b", Value: str("2")}, {Name: "a", Value: str("1")}})
	b := KeyJSON([]Column{{Name: "a", Value: str("1")}, {Name: "b", Value: str("2")}})
	if a != b || a != `{"a":"1","b":"2"}` {
		t.Fatalf("not canonical: %s vs %s", a, b)
	}
}

func TestRowKeyPrefersOldKey(t *testing.T) {
	c := Change{New: []Column{{Name: "id", Key: true, Value: str("2")}}, Old: []Column{{Name: "id", Key: true, Value: str("1")}}}
	if k := KeyJSON(c.RowKey()); k != `{"id":"1"}` {
		t.Fatalf("RowKey = %s, want the old key (the row being changed)", k)
	}
}

func TestPartialFlagOmittedWhenFinal(t *testing.T) {
	raw, _ := json.Marshal(Tx{Seq: 1, LSN: "0/1"})
	var m map[string]any
	json.Unmarshal(raw, &m)
	if _, ok := m["partial"]; ok {
		t.Fatal("final transactions must not carry partial (older receivers treat missing as final)")
	}
}
