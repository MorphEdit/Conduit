// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"errors"
	"testing"
)

func TestPairingProof(t *testing.T) {
	const fp = "ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	proof := PairingProof("123 456", "req1", fp, "cdt1_x")

	if !ProofMatches(proof, "123456", "req1", fp, "cdt1_x") {
		t.Fatal("the right code must prove the approval (spaces and fingerprint case do not matter)")
	}
	bad := []struct{ name, code, req, fp, invite string }{
		{"wrong code", "123457", "req1", fp, "cdt1_x"},
		{"another request", "123456", "req2", fp, "cdt1_x"},
		{"another approving site", "123456", "req1", "00" + fp[2:], "cdt1_x"},
		{"swapped invite", "123456", "req1", fp, "cdt1_y"},
	}
	for _, c := range bad {
		if ProofMatches(proof, c.code, c.req, c.fp, c.invite) {
			t.Errorf("%s: proof must not match", c.name)
		}
	}
	if ProofMatches("", "123456", "req1", fp, "cdt1_x") {
		t.Error("an approval without a proof (older or fake site) must not be accepted")
	}
	if ProofMatches(PairingProof("", "req1", fp, "x"), "", "req1", fp, "x") {
		t.Error("an empty code must never match")
	}
}

func TestPairingCommit(t *testing.T) {
	h := HashSecret("0123456789abcdef")
	c := PairingCommit("654 321", h)
	if !CommitMatches(c, "654321", h) {
		t.Fatal("the code shown on the site must match its commit")
	}
	if CommitMatches(c, "654322", h) || CommitMatches(c, "654321", HashSecret("another secret")) {
		t.Error("another code or another request must not match")
	}
	if CommitMatches("", "654321", h) || CommitMatches(PairingCommit("", h), "", h) {
		t.Error("an empty commit or code must never match")
	}
}

func TestRequestsApproveWithCode(t *testing.T) {
	const secret = "0123456789abcdef"
	r := NewRequests()
	id, err := r.Add("10.0.0.5", "branch", "https://branch:7443", "", PairingCommit("654321", HashSecret(secret)), secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Check(id, "12"); err == nil {
		t.Error("a code that is not 6 digits is refused")
	}
	if _, err := r.Check(id, "111111"); !errors.Is(err, ErrCodeMismatch) {
		t.Errorf("a code other than the one shown on the site is refused: got %v", err)
	}
	if err := r.Approve(id, "111111", "cdt1_inv", "fp"); !errors.Is(err, ErrCodeMismatch) {
		t.Errorf("approve must check the code too: got %v", err)
	}
	site, err := r.Check(id, "654 321")
	if err != nil || site != "branch" {
		t.Fatalf("the right code: site %q, err %v", site, err)
	}
	if err := r.Approve(id, "654321", "cdt1_inv", "fp"); err != nil {
		t.Fatal(err)
	}
	status, invite, proof, ok := r.Poll(id, secret)
	if !ok || status != "approved" || invite != "cdt1_inv" || !ProofMatches(proof, "654321", id, "fp", "cdt1_inv") {
		t.Fatalf("poll = %q %q %q %v", status, invite, proof, ok)
	}

	// A site older than v0.3 still sends its code: a typo is caught here.
	old, _ := r.Add("10.0.0.6", "office", "https://office:7443", "111 222", "", "fedcba9876543210")
	if _, err := r.Check(old, "111223"); !errors.Is(err, ErrCodeMismatch) {
		t.Errorf("mismatched legacy code: got %v", err)
	}
	if _, err := r.Check(old, "111222"); err != nil {
		t.Errorf("matching legacy code: %v", err)
	}

	// v0.3.0 sent neither: nothing could tell it from a fake site.
	if _, err := r.Add("10.0.0.7", "shop", "https://shop:7443", "", "", "aaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNoPairingProof) {
		t.Errorf("request without code or commit: got %v", err)
	}
}

// A fake site on the LAN asks to join under a real site's name. The admin
// types the code shown on the real site: the fake request must not get the
// invite, and the real one must.
func TestFakeSiteSameNameGetsNoInvite(t *testing.T) {
	const realSecret, fakeSecret = "real-secret-0123456789", "fake-secret-0123456789"
	r := NewRequests()
	fake, _ := r.Add("10.0.0.66", "branch", "https://evil:7443", "", PairingCommit("000000", HashSecret(fakeSecret)), fakeSecret)
	real, _ := r.Add("10.0.0.5", "branch", "https://branch:7443", "", PairingCommit("482915", HashSecret(realSecret)), realSecret)

	if _, err := r.Check(fake, "482915"); !errors.Is(err, ErrCodeMismatch) {
		t.Fatalf("fake request accepted the real site's code: %v", err)
	}
	if err := r.Approve(fake, "482915", "cdt1_inv", "fp"); err == nil {
		t.Fatal("fake request was approved")
	}
	if status, invite, _, _ := r.Poll(fake, fakeSecret); status != "pending" || invite != "" {
		t.Fatalf("fake request got %q %q", status, invite)
	}
	if _, err := r.Check(real, "482915"); err != nil {
		t.Fatalf("real request: %v", err)
	}
}

func TestRequestRejectedAfterTooManyWrongCodes(t *testing.T) {
	const secret = "0123456789abcdef"
	r := NewRequests()
	id, _ := r.Add("10.0.0.5", "branch", "https://branch:7443", "", PairingCommit("654321", HashSecret(secret)), secret)
	for i := 0; i < maxCodeMisses; i++ {
		r.Check(id, "000000")
	}
	if _, err := r.Check(id, "654321"); err == nil {
		t.Fatal("the request should be closed after too many wrong codes")
	}
	if status, _, _, _ := r.Poll(id, secret); status != "rejected" {
		t.Fatalf("status = %q, want rejected", status)
	}
}
