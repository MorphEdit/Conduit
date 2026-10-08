// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package apply

import (
	"log/slog"
	"testing"

	"github.com/MorphEdit/conduit/internal/config"
)

func TestSyncableSchemas(t *testing.T) {
	cfg := &config.Config{Capture: config.CaptureConfig{Schemas: []string{"public", "sales"}}}
	a := New(cfg, slog.Default())
	cases := map[string]bool{
		"public":             true,
		"sales":              true,
		"hr":                 false, // not synced on this site
		"conduit":            false, // Conduit's own members, secrets and queues
		"pg_catalog":         false, // roles, passwords, everything
		"pg_toast":           false,
		"information_schema": false,
		"":                   false,
	}
	for schema, want := range cases {
		if got := a.Syncable(schema); got != want {
			t.Errorf("Syncable(%q) = %v, want %v", schema, got, want)
		}
	}

	// Even a misconfigured site that lists them must not accept them.
	cfg.Capture.Schemas = []string{"public", "conduit", "pg_catalog"}
	for _, s := range []string{"conduit", "pg_catalog"} {
		if a.Syncable(s) {
			t.Errorf("Syncable(%q) must stay false", s)
		}
	}
}
