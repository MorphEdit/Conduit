// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package cluster

import (
	"crypto/subtle"
	"errors"
	"sort"
	"sync"
	"time"
)

// PendingJoin is a site found on the LAN asking to be let in. An admin
// types the code shown on the new site when approving (see PairingSeal).
type PendingJoin struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	URL       string    `json:"url"`
	Status    string    `json:"status"` // pending | approved | rejected
	CreatedAt time.Time `json:"created_at"`
	From      string    `json:"from"` // requester's IP address

	secretHash string
	pake       []byte // the new site's half of the pairing exchange
	reply      []byte // ours, once approved
	box        []byte // the invite, encrypted for the site that knows the code
}

// ErrNoPairingProof means a request carries no pairing exchange (a site older
// than v0.5). v0.4 sent an HMAC of its 6-digit code, which whoever received
// it could brute-force offline, so those sites are refused too.
var ErrNoPairingProof = errors.New("this site's Conduit is too old for LAN pairing: update it, or join with an invite code")

// Requests holds join requests in memory; a restart simply makes the new
// site ask again.
type Requests struct {
	mu sync.Mutex
	m  map[string]*PendingJoin
}

const (
	maxPending      = 50
	maxPendingPerIP = 2
	requestTTL      = 15 * time.Minute
)

// ErrTooMany means the request queue (or this address's share of it) is full.
var ErrTooMany = errors.New("too many pending join requests")

func NewRequests() *Requests { return &Requests{m: map[string]*PendingJoin{}} }

func (r *Requests) gc() {
	for id, p := range r.m {
		if time.Since(p.CreatedAt) > requestTTL {
			delete(r.m, id)
		}
	}
}

// Add records a request from the given address, replacing any earlier one
// from the same site. Each address may have only a couple pending, so one
// machine cannot fill the queue and lock real sites out.
func (r *Requests) Add(from, nodeID, url string, pake []byte, secret string) (string, error) {
	if len(pake) == 0 {
		return "", ErrNoPairingProof
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gc()
	fromIP := 0
	for id, p := range r.m {
		if p.Status != "pending" {
			continue
		}
		if p.NodeID == nodeID && p.From == from {
			delete(r.m, id)
			continue
		}
		if p.From == from {
			fromIP++
		}
	}
	if fromIP >= maxPendingPerIP || len(r.m) >= maxPending {
		return "", ErrTooMany
	}
	id := randomHex(8)
	r.m[id] = &PendingJoin{ID: id, NodeID: nodeID, URL: url, Status: "pending", CreatedAt: time.Now(),
		From: from, secretHash: hashSecret(secret), pake: pake}
	return id, nil
}

// Poll lets the requesting site learn the outcome; it proves it made the
// request with the secret only it knows. An approval carries our half of the
// pairing exchange and the sealed invite (see PairingOpen).
func (r *Requests) Poll(id, secret string) (status string, reply, box []byte, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, found := r.m[id]
	if !found || subtle.ConstantTimeCompare([]byte(p.secretHash), []byte(hashSecret(secret))) != 1 {
		return "", nil, nil, false
	}
	if p.Status != "pending" {
		delete(r.m, id)
	}
	return p.Status, p.reply, p.box, true
}

var errNoRequest = errors.New("no such pending request")

// Site returns the name a pending request asks to join as, for the invite
// that only that site may redeem.
func (r *Requests) Site(id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok || p.Status != "pending" {
		return "", errNoRequest
	}
	return p.NodeID, nil
}

// Approve answers the request with the code the admin typed and seals the
// invite for it. The approving site cannot tell a mistyped code from the right
// one: only the site showing the code can open the invite. On a wrong code the
// new site shows a fresh one and asks again.
func (r *Requests) Approve(id, code, invite, selfFP string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok || p.Status != "pending" {
		return errNoRequest
	}
	reply, box, err := PairingSeal(code, p.NodeID, selfFP, p.pake, id, invite)
	if err != nil {
		return err
	}
	p.Status, p.reply, p.box = "approved", reply, box
	return nil
}

func (r *Requests) Reject(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok || p.Status != "pending" {
		return errNoRequest
	}
	p.Status = "rejected"
	return nil
}

func (r *Requests) Pending() []PendingJoin {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gc()
	out := []PendingJoin{}
	for _, p := range r.m {
		if p.Status == "pending" {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}
