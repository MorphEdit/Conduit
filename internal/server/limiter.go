// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Wrong admin passwords allowed from one address within lockWindow before it
// is locked out for lockWindow. The dashboard is plain HTTP with no account
// to lock, so this is what stops someone on the LAN guessing the password.
const (
	maxPasswordFails = 10
	lockWindow       = 15 * time.Minute
	maxTracked       = 10000
)

type attempts struct {
	fails int
	first time.Time
	until time.Time
}

type limiter struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]*attempts
}

func newLimiter() *limiter { return &limiter{now: time.Now, m: map[string]*attempts{}} }

// locked reports whether ip may not try a password right now.
func (l *limiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.m[ip]
	return a != nil && l.now().Before(a.until)
}

// fail counts a wrong password from ip.
func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.m) >= maxTracked {
		for k, a := range l.m {
			if now.Sub(a.first) > lockWindow && !now.Before(a.until) {
				delete(l.m, k)
			}
		}
	}
	a := l.m[ip]
	if a == nil || now.Sub(a.first) > lockWindow {
		a = &attempts{first: now}
		l.m[ip] = a
	}
	if a.fails++; a.fails >= maxPasswordFails {
		a.until = now.Add(lockWindow)
	}
}

// ok forgets ip's wrong passwords after a right one.
func (l *limiter) ok(ip string) {
	l.mu.Lock()
	delete(l.m, ip)
	l.mu.Unlock()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isLoopback(r *http.Request) bool {
	ip := net.ParseIP(remoteIP(r))
	return ip != nil && ip.IsLoopback()
}
