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
// dashboard and never sends it anywhere. The admin types that code on the
// approving site, which returns PairingProof with the invite. The new site
// accepts the invite only if the proof matches, so a fake site that
// answered the LAN broadcast cannot approve the join (it never saw the code),
// and an approval cannot be replayed to another request, site or invite.

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

// ProofMatches checks an approval received by the site waiting to join.
func ProofMatches(proof, code, requestID, seedFP, invite string) bool {
	if proof == "" || len(NormalizeCode(code)) != 6 {
		return false
	}
	return hmac.Equal([]byte(proof), []byte(PairingProof(code, requestID, seedFP, invite)))
}
