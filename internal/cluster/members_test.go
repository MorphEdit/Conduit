// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func member(id, url, fp, status string, at time.Time) Member {
	return Member{ID: id, URL: url, CertFP: fp, KeyHash: "k-" + id, Status: status, Offset: 1, UpdatedAt: at}
}

func TestDecideTrustRules(t *testing.T) {
	host := member("host", "https://host:7443", "aaa", "active", t0)
	later := t0.Add(time.Hour)
	forged := member("host", "https://evil:7443", "000", "active", later)

	cases := []struct {
		name     string
		local    *Member
		remote   Member
		self     string
		from     string
		wantOK   bool
		wantURL  string
		wantStat string
	}{
		{"third party cannot change a site's address", &host, forged, "branch", "local", false, "", ""},
		{"the site itself can change its address", &host, forged, "branch", "host", true, "https://evil:7443", "active"},
		{"nobody can rewrite our own record", &host, forged, "host", "local", false, "", ""},
		{"not even a site claiming to be us", &host, forged, "host", "host", false, "", ""},
		{"third party may pass on a removal (identity kept)", &host,
			member("host", "https://evil:7443", "000", "removed", later), "branch", "local", true, "https://host:7443", "removed"},
		{"we accept being removed (identity kept)", &host,
			member("host", "https://evil:7443", "000", "removed", later), "host", "local", true, "https://host:7443", "removed"},
		{"older record is ignored", &host, member("host", "https://x", "1", "active", t0.Add(-time.Hour)), "branch", "host", false, "", ""},
		{"a joining site learns its own record from the seed", nil, member("branch", "https://b:7443", "ccc", "active", later), "branch", "host", true, "https://b:7443", "active"},
		{"unknown site is learned as is", nil, member("newsite", "https://n:7443", "bbb", "active", later), "branch", "host", true, "https://n:7443", "active"},
		{"no resurrection by a third party", ptr(member("host", "https://host:7443", "aaa", "removed", t0)),
			member("host", "https://host:7443", "aaa", "active", later), "branch", "local", false, "", ""},
		{"invalid id is ignored", nil, member("BAD ID", "https://x", "c", "active", later), "branch", "host", false, "", ""},
	}
	for _, c := range cases {
		got, ok := decide(c.local, c.remote, c.self, c.from)
		if ok != c.wantOK {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.wantOK)
			continue
		}
		if ok && (got.URL != c.wantURL || got.Status != c.wantStat) {
			t.Errorf("%s: got url=%s status=%s, want url=%s status=%s", c.name, got.URL, got.Status, c.wantURL, c.wantStat)
		}
	}
}

func ptr(m Member) *Member { return &m }

func TestSiteCannotChangeItsOwnIDSlot(t *testing.T) {
	local := member("host", "https://host:7443", "aaa", "active", t0)
	remote := member("host", "https://host2:7443", "aaa", "active", t0.Add(time.Hour))
	remote.Offset = 7 // would collide with another site's ids
	got, ok := decide(&local, remote, "branch", "host")
	if !ok || got.URL != "https://host2:7443" || got.Offset != 1 {
		t.Fatalf("got ok=%v url=%s offset=%d; want the new URL with the original slot 1", ok, got.URL, got.Offset)
	}
}

func TestJoinRequestLimits(t *testing.T) {
	r := NewRequests()
	secret := "0123456789abcdef"
	for i := 0; i < maxPendingPerIP; i++ {
		if _, err := r.Add("10.0.0.5", "site"+string(rune('a'+i)), "https://x", "111 111", secret); err != nil {
			t.Fatalf("request %d from one address refused: %v", i, err)
		}
	}
	if _, err := r.Add("10.0.0.5", "sitez", "https://x", "111 111", secret); err != ErrTooMany {
		t.Fatalf("expected ErrTooMany for a 3rd request from the same address, got %v", err)
	}
	if _, err := r.Add("10.0.0.9", "real", "https://real", "222 222", secret); err != nil {
		t.Fatalf("a different address must still get in: %v", err)
	}
	// the same site asking again replaces its request instead of piling up
	if _, err := r.Add("10.0.0.9", "real", "https://real", "333 333", secret); err != nil {
		t.Fatalf("repeat request refused: %v", err)
	}
	if n := len(r.Pending()); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}
}
