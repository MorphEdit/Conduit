// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"errors"
	"testing"
)

const (
	seedFP = "ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	fakeFP = "00cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

func TestPairingOpensWithTheRightCode(t *testing.T) {
	msg, st, err := PairingStart("123 456", "branch", seedFP)
	if err != nil {
		t.Fatal(err)
	}
	reply, box, err := PairingSeal("123456", "branch", seedFP, msg, "req1", "cdt1_x")
	if err != nil {
		t.Fatal(err)
	}
	invite, err := PairingOpen(st, reply, box, "req1")
	if err != nil || invite != "cdt1_x" {
		t.Fatalf("the right code must open the invite (spaces and fingerprint case do not matter): %q %v", invite, err)
	}
}

func TestPairingRefusesEverythingElse(t *testing.T) {
	bad := []struct{ name, code, node, fp, req string }{
		{"wrong code", "123457", "branch", seedFP, "req1"},
		{"another site name", "123456", "shop", seedFP, "req1"},
		// A fake site in the middle relays the request to the real one: the
		// new site paired with the fake site's certificate.
		{"another approving site", "123456", "branch", fakeFP, "req1"},
		{"another request", "123456", "branch", seedFP, "req2"},
	}
	for _, c := range bad {
		msg, st, _ := PairingStart("123456", "branch", seedFP)
		reply, box, err := PairingSeal(c.code, c.node, c.fp, msg, c.req, "cdt1_x")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if invite, err := PairingOpen(st, reply, box, "req1"); !errors.Is(err, ErrBadPairing) || invite != "" {
			t.Errorf("%s: opened %q, err %v", c.name, invite, err)
		}
	}
	_, st, _ := PairingStart("123456", "branch", seedFP)
	if _, err := PairingOpen(st, nil, nil, "req1"); !errors.Is(err, ErrBadPairing) {
		t.Error("an approval without a reply (older or fake site) must not be accepted")
	}
	if _, _, err := PairingStart("", "branch", seedFP); err == nil {
		t.Error("an empty code must never be used")
	}
	if _, _, err := PairingSeal("12", "branch", seedFP, []byte("x"), "req1", "cdt1_x"); err == nil {
		t.Error("a code that is not 6 digits is refused")
	}
}

func TestRequestsApproveWithCode(t *testing.T) {
	const secret = "0123456789abcdef"
	msg, st, _ := PairingStart("654321", "branch", seedFP)
	r := NewRequests()
	id, err := r.Add("10.0.0.5", "branch", "https://branch:7443", msg, secret)
	if err != nil {
		t.Fatal(err)
	}
	if site, err := r.Site(id); err != nil || site != "branch" {
		t.Fatalf("site %q, err %v", site, err)
	}
	if err := r.Approve(id, "654 321", "cdt1_inv", seedFP); err != nil {
		t.Fatal(err)
	}
	status, reply, box, ok := r.Poll(id, secret)
	if !ok || status != "approved" {
		t.Fatalf("poll = %q %v", status, ok)
	}
	if invite, err := PairingOpen(st, reply, box, id); err != nil || invite != "cdt1_inv" {
		t.Fatalf("open = %q %v", invite, err)
	}
	if _, _, _, ok := r.Poll(id, "wrong secret"); ok {
		t.Error("another secret must not see the request")
	}

	// Older sites sent no pairing exchange: nothing could tell them from a fake site.
	if _, err := r.Add("10.0.0.7", "shop", "https://shop:7443", nil, "aaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNoPairingProof) {
		t.Errorf("request without a pairing exchange: got %v", err)
	}
}

// A fake site on the LAN asks to join under a real site's name. The admin
// types the code shown on the real site into the fake request: the fake site
// must not be able to open the invite.
func TestFakeSiteSameNameGetsNoInvite(t *testing.T) {
	const fakeSecret = "fake-secret-0123456789"
	fakeMsg, fakeSt, _ := PairingStart("000000", "branch", seedFP) // its guess
	r := NewRequests()
	fake, _ := r.Add("10.0.0.66", "branch", "https://evil:7443", fakeMsg, fakeSecret)
	if err := r.Approve(fake, "482915", "cdt1_inv", seedFP); err != nil {
		t.Fatal(err)
	}
	_, reply, box, _ := r.Poll(fake, fakeSecret)
	if invite, err := PairingOpen(fakeSt, reply, box, fake); err == nil {
		t.Fatalf("fake request opened the invite: %q", invite)
	}
}
