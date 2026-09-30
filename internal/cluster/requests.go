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
// compares Code with the one shown on the new site, then approves.
type PendingJoin struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	URL       string    `json:"url"`
	Code      string    `json:"code"`
	Status    string    `json:"status"` // pending | approved | rejected
	CreatedAt time.Time `json:"created_at"`

	secretHash string
	invite     string
}

// Requests holds join requests in memory; a restart simply makes the new
// site ask again.
type Requests struct {
	mu sync.Mutex
	m  map[string]*PendingJoin
}

const (
	maxPending = 20
	requestTTL = time.Hour
)

func NewRequests() *Requests { return &Requests{m: map[string]*PendingJoin{}} }

func (r *Requests) gc() {
	for id, p := range r.m {
		if time.Since(p.CreatedAt) > requestTTL {
			delete(r.m, id)
		}
	}
}

// Add records a request, replacing any earlier one from the same site.
func (r *Requests) Add(nodeID, url, code, secret string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gc()
	for id, p := range r.m {
		if p.NodeID == nodeID && p.Status == "pending" {
			delete(r.m, id)
		}
	}
	if len(r.m) >= maxPending {
		return "", errors.New("too many pending join requests")
	}
	id := randomHex(8)
	r.m[id] = &PendingJoin{ID: id, NodeID: nodeID, URL: url, Code: code, Status: "pending", CreatedAt: time.Now(), secretHash: hashSecret(secret)}
	return id, nil
}

// Poll lets the requesting site learn the outcome; it proves it made the
// request with the secret only it knows.
func (r *Requests) Poll(id, secret string) (status, invite string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, found := r.m[id]
	if !found || subtle.ConstantTimeCompare([]byte(p.secretHash), []byte(hashSecret(secret))) != 1 {
		return "", "", false
	}
	if p.Status != "pending" {
		delete(r.m, id)
	}
	return p.Status, p.invite, true
}

func (r *Requests) Decide(id string, approve bool, invite string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	if !ok || p.Status != "pending" {
		return errors.New("no such pending request")
	}
	if approve {
		p.Status, p.invite = "approved", invite
	} else {
		p.Status = "rejected"
	}
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
