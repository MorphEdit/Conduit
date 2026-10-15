// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"strings"

	"filippo.io/cpace"
)

// LAN pairing. A site waiting to join shows a 6-digit code on its own
// dashboard; the admin types it when approving on another site. The code is
// the password of a CPace exchange (a PAKE):
//
//   - The new site sends PairingStart's message with its request.
//   - The approving site answers with PairingSeal: its half of the exchange
//     and the invite, encrypted under the key the exchange derives.
//   - The new site opens it with PairingOpen.
//
// Only a party that knows the code derives the same key, and nothing either
// side sends lets anyone test guesses offline: each request is worth one
// guess at most. (A plain HMAC keyed by six digits could be brute-forced in
// a second by whoever received it, including a fake site answering the LAN
// broadcast.) The exchange is bound to the new site's name and to the
// certificate of the site it is talking to, so a fake site in the middle that
// relays the request to a real one gets nothing either side can use.

const pairingContext = "conduit-pair-v2"

// ErrBadPairing means an approval could not be opened: a mistyped code, or
// a fake site on the LAN.
var ErrBadPairing = errors.New("join approval did not prove the pairing code")

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

// ValidCode reports whether s holds a 6-digit pairing code.
func ValidCode(s string) bool { return len(NormalizeCode(s)) == 6 }

func pairingInfo(nodeID, seedFP string) *cpace.ContextInfo {
	return cpace.NewContextInfo(nodeID, strings.ToLower(seedFP), []byte(pairingContext))
}

// PairingStart begins pairing on the new site, which asks to join as nodeID
// through the site whose certificate is seedFP. msg goes with the request;
// st opens the approval.
func PairingStart(code, nodeID, seedFP string) (msg []byte, st *cpace.State, err error) {
	if !ValidCode(code) {
		return nil, nil, errors.New("pairing code must have 6 digits")
	}
	return cpace.Start(NormalizeCode(code), pairingInfo(nodeID, seedFP))
}

// PairingSeal answers a request's msg on the approving site (whose
// certificate is selfFP) with the code the admin typed, and encrypts the
// invite under the derived key.
func PairingSeal(code, nodeID, selfFP string, msg []byte, requestID, invite string) (reply, box []byte, err error) {
	if !ValidCode(code) {
		return nil, nil, errors.New("type the 6-digit pairing code shown on the new site")
	}
	reply, key, err := cpace.Exchange(NormalizeCode(code), pairingInfo(nodeID, selfFP), msg)
	if err != nil {
		return nil, nil, err
	}
	aead, err := pairingAEAD(key, requestID)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	rand.Read(nonce)
	return reply, aead.Seal(nonce, nonce, []byte(invite), []byte(requestID)), nil
}

// PairingOpen finishes pairing on the new site and returns the invite.
func PairingOpen(st *cpace.State, reply, box []byte, requestID string) (string, error) {
	if st == nil || len(reply) == 0 {
		return "", ErrBadPairing
	}
	key, err := st.Finish(reply)
	if err != nil {
		return "", ErrBadPairing
	}
	aead, err := pairingAEAD(key, requestID)
	if err != nil {
		return "", err
	}
	if len(box) < aead.NonceSize() {
		return "", ErrBadPairing
	}
	invite, err := aead.Open(nil, box[:aead.NonceSize()], box[aead.NonceSize():], []byte(requestID))
	if err != nil {
		return "", ErrBadPairing
	}
	return string(invite), nil
}

func pairingAEAD(key []byte, requestID string) (cipher.AEAD, error) {
	k, err := hkdf.Expand(sha256.New, key, pairingContext+" invite|"+requestID, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
