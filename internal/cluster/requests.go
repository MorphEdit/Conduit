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
// types the code shown on the new site when approving (see PairingCommit).
type PendingJoin struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	URL       string    `json:"url"`
	Status    string    `json:"status"` // pending | approved | rejected
	CreatedAt time.Time `json:"created_at"`
	From      string    `json:"from"` // requester's IP address

	secretHash string
	commit     string // never exported: the code could be guessed from it
	misses     int
	invite     string
	proof      string
}

// ErrCodeMismatch means the typed pairing code is not the one the site sent.
var ErrCodeMismatch = errors.New("pairing code does not match the one shown on the new site")

// ErrNoPairingProof means a request carries no commit to its pairing code
// (a site older than v0.4), so nothing could tell it apart from a fake site
// asking under the same name. Sites older than v0.3 sent the code itself,
// which anyone could read from /status and reuse, so they are refused too.
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
	// A request is rejected after this many wrong codes; the new site asks
	// again later with a fresh code.
	maxCodeMisses = 5
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
func (r *Requests) Add(from, nodeID, url, commit, secret string) (string, error) {
	if commit == "" {
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
		From: from, secretHash: hashSecret(secret), commit: commit}
	return id, nil
}

// Poll lets the requesting site learn the outcome; it proves it made the
// request with the secret only it knows.
func (r *Requests) Poll(id, secret string) (status, invite, proof string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, found := r.m[id]
	if !found || subtle.ConstantTimeCompare([]byte(p.secretHash), []byte(hashSecret(secret))) != 1 {
		return "", "", "", false
	}
	if p.Status != "pending" {
		delete(r.m, id)
	}
	return p.Status, p.invite, p.proof, true
}

var errNoRequest = errors.New("no such pending request")

// Check validates a typed pairing code before an approval and returns the
// requesting site's name: only the site showing that code (the one that sent
// the matching commit) can receive the invite. Too many wrong codes reject
// the request.
func (r *Requests) Check(id, code string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok || p.Status != "pending" {
		return "", errNoRequest
	}
	if len(NormalizeCode(code)) != 6 {
		return "", errors.New("type the 6-digit pairing code shown on the new site")
	}
	if !p.matches(code) {
		if p.misses++; p.misses >= maxCodeMisses {
			p.Status = "rejected"
		}
		return "", ErrCodeMismatch
	}
	return p.NodeID, nil
}

func (p *PendingJoin) matches(code string) bool {
	return CommitMatches(p.commit, code, p.secretHash)
}

// Approve hands the request its invite, with the proof that the approving
// admin knew the code shown on the new site. The code is checked again here,
// so an invite never reaches a request whose code was not typed.
func (r *Requests) Approve(id, code, invite, seedFP string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok || p.Status != "pending" {
		return errNoRequest
	}
	if !p.matches(code) {
		return ErrCodeMismatch
	}
	p.Status, p.invite, p.proof = "approved", invite, PairingProof(code, id, seedFP, invite)
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
