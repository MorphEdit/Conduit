// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClockSkew(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	end := start.Add(400 * time.Millisecond) // round trip 400 ms: up to 200 ms is travel
	cases := []struct {
		remote time.Time
		want   time.Duration
	}{
		{start.Add(200 * time.Millisecond), 0},                                     // in sync
		{start.Add(350 * time.Millisecond), 0},                                     // within the round trip
		{start.Add(5*time.Second + 200*time.Millisecond), 4800 * time.Millisecond}, // 5 s ahead, 200 ms could be travel
		{start.Add(-3 * time.Second), -3000 * time.Millisecond},                    // 3.2 s behind the midpoint, minus 200 ms
	}
	for _, c := range cases {
		if got := clockSkew(c.remote, start, end); got != c.want {
			t.Errorf("clockSkew(%v) = %v, want %v", c.remote.Sub(start), got, c.want)
		}
	}
}

func TestDashboardSecurityHeaders(t *testing.T) {
	// The inline import map must be allowed by hash, nothing else inline.
	if n := strings.Count(dashboardCSP, "'sha256-"); n != 1 {
		t.Fatalf("CSP should allow exactly the import map by hash, got %d hashes: %s", n, dashboardCSP)
	}
	if strings.Contains(dashboardCSP, "unsafe-inline") || strings.Contains(dashboardCSP, "unsafe-eval") {
		t.Fatalf("CSP must not allow inline or eval'd scripts: %s", dashboardCSP)
	}
	h := secureHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("Content-Security-Policy") != dashboardCSP || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v", rec.Header())
	}
}
