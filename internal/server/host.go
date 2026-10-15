// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package server

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// DNS rebinding: a web page on another site can point its own domain name at
// this dashboard's address, and the visitor's browser then treats the
// dashboard as part of that page (same origin), so the page can read /status
// and, from a browser on this machine without an admin password, the pairing
// code. Its requests carry the attacker's name in Host. The dashboard
// therefore answers only to names it is known by: an IP address, localhost,
// this machine's name, the advertised name, and any listed in dashboard_hosts
// (e.g. a reverse proxy's public name).

// dashboardHosts returns the lowercase host names the dashboard answers to.
func dashboardHosts(advertise string, extra []string) map[string]bool {
	ok := map[string]bool{"localhost": true}
	add := func(h string) {
		h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
		if h != "" {
			ok[h] = true
			if short, _, found := strings.Cut(h, "."); found {
				ok[short] = true
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	if u, err := url.Parse(advertise); err == nil {
		add(u.Hostname())
	}
	for _, h := range extra {
		add(h)
	}
	return ok
}

// knownHost reports whether a request's Host header names this dashboard.
func knownHost(host string, names map[string]bool) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || names[host] || strings.HasSuffix(host, ".localhost") {
		return true
	}
	return false
}

func (s *Server) checkHost(h http.Handler) http.Handler {
	names := dashboardHosts(s.cfg.Advertise, s.cfg.DashboardHosts)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !knownHost(r.Host, names) {
			s.log.Warn("dashboard request for an unknown host name refused (add it to CONDUIT_DASHBOARD_HOSTS if it is yours)", "host", r.Host, "from", remoteIP(r))
			http.Error(w, "unknown host name: add it to CONDUIT_DASHBOARD_HOSTS", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}
