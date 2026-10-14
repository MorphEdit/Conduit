// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiterLocksOutPasswordGuessing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < maxPasswordFails-1; i++ {
		l.fail("10.0.0.5")
	}
	if l.locked("10.0.0.5") {
		t.Fatal("locked before the limit")
	}
	l.fail("10.0.0.5")
	if !l.locked("10.0.0.5") {
		t.Fatal("not locked after too many wrong passwords")
	}
	if l.locked("10.0.0.6") {
		t.Fatal("another address must not be locked")
	}
	now = now.Add(lockWindow + time.Second)
	if l.locked("10.0.0.5") {
		t.Fatal("still locked after the window")
	}

	// A right password clears the count.
	for i := 0; i < maxPasswordFails-1; i++ {
		l.fail("10.0.0.7")
	}
	l.ok("10.0.0.7")
	l.fail("10.0.0.7")
	if l.locked("10.0.0.7") {
		t.Fatal("count not cleared by a right password")
	}
}

func TestLoopbackOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "/status", nil)
	for addr, want := range map[string]bool{"127.0.0.1:5000": true, "[::1]:5000": true, "192.168.1.20:5000": false, "172.17.0.1:5000": false} {
		r.RemoteAddr = addr
		if got := isLoopback(r); got != want {
			t.Errorf("isLoopback(%s) = %v, want %v", addr, got, want)
		}
	}
	// A reverse proxy on this machine relays other people's requests.
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "192.168.1.20")
	if isLoopback(r) {
		t.Error("a proxied request must not count as this machine")
	}
}
