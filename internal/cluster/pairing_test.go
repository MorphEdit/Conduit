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

func TestRequestsApproveWithCode(t *testing.T) {
	r := NewRequests()
	id, err := r.Add("10.0.0.5", "branch", "https://branch:7443", "", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(id, "12"); err == nil {
		t.Error("a code that is not 6 digits is refused")
	}
	if err := r.Check(id, "654 321"); err != nil {
		t.Fatalf("new sites do not send their code, so any 6 digits pass here: %v", err)
	}
	if err := r.Approve(id, "654321", "cdt1_inv", "fp"); err != nil {
		t.Fatal(err)
	}
	status, invite, proof, ok := r.Poll(id, "0123456789abcdef")
	if !ok || status != "approved" || invite != "cdt1_inv" || !ProofMatches(proof, "654321", id, "fp", "cdt1_inv") {
		t.Fatalf("poll = %q %q %q %v", status, invite, proof, ok)
	}

	// A site older than v0.3 still sends its code: a typo is caught here.
	old, _ := r.Add("10.0.0.6", "office", "https://office:7443", "111 222", "fedcba9876543210")
	if err := r.Check(old, "111223"); !errors.Is(err, ErrCodeMismatch) {
		t.Errorf("mismatched legacy code: got %v", err)
	}
	if err := r.Check(old, "111222"); err != nil {
		t.Errorf("matching legacy code: %v", err)
	}
}
