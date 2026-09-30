// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package peer_test

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/conduit-sync/conduit/internal/cluster"
	. "github.com/conduit-sync/conduit/internal/peer"
)

func tlsServer(t *testing.T, m *cluster.TLSMaterial) *httptest.Server {
	t.Helper()
	cfg, err := m.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func TestPinnedClientAcceptsMatchingFingerprint(t *testing.T) {
	m, err := cluster.GenerateTLS("a")
	if err != nil {
		t.Fatal(err)
	}
	srv := tlsServer(t, m)
	resp, err := PinnedClient(m.Fingerprint, 5*time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("pinned request failed: %v", err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("expected TLS 1.3, got %+v", resp.TLS)
	}
}

func TestPinnedClientRejectsOtherCertificate(t *testing.T) {
	real, _ := cluster.GenerateTLS("real")
	impostor, _ := cluster.GenerateTLS("impostor")
	srv := tlsServer(t, impostor)
	_, err := PinnedClient(real.Fingerprint, 5*time.Second).Get(srv.URL)
	if err == nil || !errors.Is(err, ErrFingerprint) {
		t.Fatalf("expected fingerprint error, got %v", err)
	}
}

func TestPinnedClientRejectsEmptyPin(t *testing.T) {
	m, _ := cluster.GenerateTLS("a")
	srv := tlsServer(t, m)
	if _, err := PinnedClient("", 5*time.Second).Get(srv.URL); err == nil {
		t.Fatal("empty pin must never be accepted")
	}
}

func TestParseAuth(t *testing.T) {
	c := Credentials{ID: "local", Secret: "s3cret"}
	id, secret, ok := ParseAuth(c.Header())
	if !ok || id != "local" || secret != "s3cret" {
		t.Fatalf("round trip failed: %q %q %v", id, secret, ok)
	}
	for _, bad := range []string{"", "Bearer x", "Conduit ", "Conduit id", "Conduit :x", "Conduit id:"} {
		if _, _, ok := ParseAuth(bad); ok {
			t.Errorf("ParseAuth(%q) should fail", bad)
		}
	}
}
