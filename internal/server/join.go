// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/MorphEdit/conduit/internal/cluster"
	"github.com/MorphEdit/conduit/internal/config"
	"github.com/MorphEdit/conduit/internal/pgcli"
)

const inviteTTL = 24 * time.Hour

// join redeems an invite: the new site becomes a member and gets the
// cluster token, its id slot and the current member list.
func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	v, ok := s.running(w)
	if !ok {
		return
	}
	var req cluster.JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || !config.ValidID(req.ID) || req.URL == "" {
		http.Error(w, "bad join request", http.StatusBadRequest)
		return
	}
	resp, err := cluster.Admit(r.Context(), v.Store.Pool(), v.Identity, s.rt.Credentials(), req)
	switch {
	case errors.Is(err, cluster.ErrBadInvite):
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case errors.Is(err, cluster.ErrNameTaken):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("admitted new site", "site", req.ID, "url", req.URL, "offset", resp.Offset)
	v.Manager.Trigger()
	writeJSON(w, resp)
}

// joinRequest records a LAN site asking to join, for an admin to approve.
func (s *Server) joinRequest(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.running(w); !ok {
		return
	}
	// LAN discovery is for the local network only; other sites use invites.
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil || !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		http.Error(w, "join requests are only accepted from the local network; use an invite code", http.StatusForbidden)
		return
	}
	var body struct {
		NodeID string `json:"node_id"`
		URL    string `json:"url"`
		Code   string `json:"code"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil ||
		!config.ValidID(body.NodeID) || body.URL == "" || len(body.Code) > 16 || len(body.Secret) < 16 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id, err := s.rt.Requests.Add(host, body.NodeID, body.URL, body.Code, body.Secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	s.log.Info("join request waiting for approval", "site", body.NodeID, "code", body.Code)
	writeJSON(w, map[string]string{"request_id": id})
}

func (s *Server) joinPoll(w http.ResponseWriter, r *http.Request) {
	status, invite, proof, ok := s.rt.Requests.Poll(r.PathValue("id"), r.Header.Get("X-Request-Secret"))
	if !ok {
		http.Error(w, "unknown request", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]string{"status": status, "invite": invite, "proof": proof})
}

// allocate assigns an id slot for a newcomer another member is admitting
// (only the allocator answers; see cluster.Allocator).
func (s *Server) allocate(w http.ResponseWriter, r *http.Request, caller string) {
	v := s.rt.View()
	var req cluster.JoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil ||
		!config.ValidID(req.ID) || req.URL == "" || req.KeyHash == "" || req.CertFP == "" {
		http.Error(w, "bad allocation request", http.StatusBadRequest)
		return
	}
	offset, err := cluster.AllocateFor(r.Context(), v.Store.Pool(), v.Identity, req)
	switch {
	case errors.Is(err, cluster.ErrNameTaken), errors.Is(err, cluster.ErrNotAllocator):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("assigned an id slot", "site", req.ID, "offset", offset, "admitted_by", caller)
	v.Manager.Trigger()
	writeJSON(w, map[string]int64{"offset": offset})
}

// schema returns this site's table definitions for a new, empty site.
func (s *Server) schema(w http.ResponseWriter, r *http.Request, _ string) {
	args := []string{"--schema-only", "--no-owner", "--no-privileges", "--no-publications", "--no-subscriptions"}
	for _, sc := range s.cfg.Capture.Schemas {
		args = append(args, "--schema="+sc)
	}
	dbname, env := pgcli.Conn(s.cfg.Database) // password goes in the environment, not the process list
	args = append(args, "--dbname="+dbname)
	cmd := exec.CommandContext(r.Context(), "pg_dump", args...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		msg := err.Error()
		if errors.As(err, &ee) {
			msg += ": " + strings.TrimSpace(string(ee.Stderr))
		}
		http.Error(w, "pg_dump failed: "+msg, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/sql")
	w.Write(out)
}

// ---------- admin ----------

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	v, ok := s.running(w)
	if !ok {
		return
	}
	code, exp, err := cluster.CreateInvite(r.Context(), v.Store.Pool(), s.cfg.Advertise, v.TLS.Fingerprint, inviteTTL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("invite created", "expires", exp)
	writeJSON(w, map[string]any{"code": code, "expires_at": exp})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	v, ok := s.running(w)
	if !ok {
		return
	}
	id, decision := r.PathValue("id"), r.PathValue("decision")
	var err error
	switch decision {
	case "approve":
		// The admin types the pairing code shown on the new site; the new
		// site only accepts an approval proven with it.
		var body struct {
			Code string `json:"code"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body)
		if err = s.rt.Requests.Check(id, body.Code); err != nil {
			break
		}
		var invite string
		if invite, _, err = cluster.CreateInvite(r.Context(), v.Store.Pool(), s.cfg.Advertise, v.TLS.Fingerprint, time.Hour); err == nil {
			err = s.rt.Requests.Approve(id, body.Code, invite, v.TLS.Fingerprint)
		}
	case "reject":
		err = s.rt.Requests.Reject(id)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("join request "+decision+"d", "request", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	v, ok := s.running(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == s.cfg.NodeID {
		http.Error(w, "remove a site from another site's dashboard", http.StatusBadRequest)
		return
	}
	if err := cluster.Remove(r.Context(), v.Store.Pool(), id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("site removed from cluster", "site", id)
	v.Manager.Trigger()
	w.WriteHeader(http.StatusNoContent)
}

// replay re-applies changes skipped for a schema mismatch.
func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	v, ok := s.running(w)
	if !ok {
		return
	}
	done, failed, err := v.Applier.ReplaySkipped(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("replayed skipped changes", "replayed", done, "still_failing", failed)
	writeJSON(w, map[string]int{"replayed": done, "still_failing": failed})
}

// configurePostgres is the admin's go-ahead to change Postgres settings.
func (s *Server) configurePostgres(w http.ResponseWriter, _ *http.Request) {
	if err := s.rt.AllowPostgresConfig(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pasteInvite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Invite string `json:"invite"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.rt.PasteInvite(body.Invite); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
