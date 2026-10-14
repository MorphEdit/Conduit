// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// LAN pairing. A site waiting to join shows a 6-digit code on its own
// dashboard and never sends the code itself anywhere. The proof works both
// ways:
//
//   - The new site sends PairingCommit with its request. The approving site
//     checks the code the admin types against it and hands out an invite only
//     if it matches, so a fake site on the LAN that asks to join under a real
//     site's name gets nothing (it never saw the code shown on the real one).
//   - The approving site returns PairingProof with the invite. The new site
//     accepts the invite only if the proof matches, so a fake site that
//     answered the LAN broadcast cannot approve the join, and an approval
//     cannot be replayed to another request, site or invite.

// NormalizeCode keeps only the digits of a typed pairing code.
func NormalizeCode(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PairingProof binds an approval to the pairing code, the join request, the
// approving site's certificate and the invite it hands out.
func PairingProof(code, requestID, seedFP, invite string) string {
	m := hmac.New(sha256.New, []byte(NormalizeCode(code)))
	m.Write([]byte("conduit-pair-v1|" + requestID + "|" + strings.ToLower(seedFP) + "|" + invite))
	return hex.EncodeToString(m.Sum(nil))
}

// PairingCommit binds a join request to the code shown on the requesting
// site. It is keyed by the hash of the request's secret, which only the
// approving site holds, and is kept in that site's memory only: six digits
// are easy to guess offline, so it must never be shown or logged.
func PairingCommit(code, secretHash string) string {
	m := hmac.New(sha256.New, []byte(NormalizeCode(code)))
	m.Write([]byte("conduit-commit-v1|" + secretHash))
	return hex.EncodeToString(m.Sum(nil))
}

// CommitMatches checks the code an admin typed against a request's commit.
func CommitMatches(commit, code, secretHash string) bool {
	if commit == "" || len(NormalizeCode(code)) != 6 {
		return false
	}
	return hmac.Equal([]byte(commit), []byte(PairingCommit(code, secretHash)))
}

// ProofMatches checks an approval received by the site waiting to join.
func ProofMatches(proof, code, requestID, seedFP, invite string) bool {
	if proof == "" || len(NormalizeCode(code)) != 6 {
		return false
	}
	return hmac.Equal([]byte(proof), []byte(PairingProof(code, requestID, seedFP, invite)))
}
