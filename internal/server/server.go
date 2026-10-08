// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Package server is Conduit's HTTP surface: peer sync, cluster membership,
// admin actions and the dashboard.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/MorphEdit/conduit/internal/buildinfo"
	"github.com/MorphEdit/conduit/internal/change"
	"github.com/MorphEdit/conduit/internal/cluster"
	"github.com/MorphEdit/conduit/internal/config"
	"github.com/MorphEdit/conduit/internal/node"
	"github.com/MorphEdit/conduit/internal/peer"
	"github.com/MorphEdit/conduit/internal/snapshot"
)

const maxBody = 64 << 20

// walWarnBytes: warn on the dashboard when the slot holds back this much WAL.
const walWarnBytes = 1 << 30

// The dashboard served at "/".
//
//go:embed ui
var uiFiles embed.FS

type Server struct {
	cfg *config.Config
	rt  *node.Runtime
	log *slog.Logger
}

func New(rt *node.Runtime) *Server {
	return &Server{cfg: rt.Cfg, rt: rt, log: rt.Log.With("component", "server")}
}

func health(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) }

// PeerHandler is served over TLS on the peer port, for other sites only.
func (s *Server) PeerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)

	// Between member sites (each authenticates with its own secret).
	mux.HandleFunc("GET /status", s.peerOnly(func(w http.ResponseWriter, r *http.Request, _ string) { s.status(w, r) }))
	mux.HandleFunc("POST /v1/apply", s.peerOnly(s.apply))
	mux.HandleFunc("GET /v1/snapshot", s.peerOnly(s.snapshot))
	mux.HandleFunc("GET /v1/members", s.peerOnly(s.members))
	mux.HandleFunc("GET /v1/schema", s.peerOnly(s.schema))
	mux.HandleFunc("POST /v1/allocate", s.peerOnly(s.allocate))

	// New sites asking to join (invite secret / request secret).
	mux.HandleFunc("POST /v1/join", s.join)
	mux.HandleFunc("POST /v1/join-requests", s.joinRequest)
	mux.HandleFunc("GET /v1/join-requests/{id}", s.joinPoll)
	return mux
}

// DashboardHandler is the local web UI and admin actions (plain HTTP).
func (s *Server) DashboardHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /v1/mesh", s.mesh)

	// Dashboard actions (admin password).
	mux.HandleFunc("GET /v1/admin/check", s.admin(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	mux.HandleFunc("POST /v1/admin/invites", s.admin(s.createInvite))
	mux.HandleFunc("POST /v1/admin/requests/{id}/{decision}", s.admin(s.decide))
	mux.HandleFunc("POST /v1/admin/members/{id}/remove", s.admin(s.removeMember))
	mux.HandleFunc("POST /v1/admin/join", s.admin(s.pasteInvite))
	mux.HandleFunc("POST /v1/admin/replay", s.admin(s.replay))
	mux.HandleFunc("POST /v1/admin/configure-postgres", s.admin(s.configurePostgres))

	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	return mux
}

// ---------- auth ----------

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// peerOnly admits requests from active member sites. Each site signs in
// with its own secret; a removed site is rejected as soon as the removal
// reaches this site by gossip.
func (s *Server) peerOnly(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.running(w)
		if !ok {
			return
		}
		id, secret, ok := peer.ParseAuth(r.Header.Get("Authorization"))
		if ok && cluster.WasRemoved(r.Context(), v.Store.Pool(), id, secret) {
			// Proven to be the removed site itself: tell it, so it stops.
			w.Header().Set(cluster.RemovedHeader, "1")
			http.Error(w, "this site was removed from the cluster", http.StatusForbidden)
			return
		}
		if !ok || cluster.Authenticate(r.Context(), v.Store.Pool(), id, secret) != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r, id)
	}
}

// isAdmin reports whether r may see admin-only status details: it carries the
// admin password, or this site has none set.
func (s *Server) isAdmin(r *http.Request) bool {
	return s.cfg.AdminPassword == "" || equal(r.Header.Get("X-Admin-Password"), s.cfg.AdminPassword)
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminPassword == "" {
			http.Error(w, "admin actions are off: set CONDUIT_ADMIN_PASSWORD on this site", http.StatusForbidden)
			return
		}
		if !equal(r.Header.Get("X-Admin-Password"), s.cfg.AdminPassword) {
			http.Error(w, "wrong admin password", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// running returns the runtime view, or answers 503 while the site starts.
func (s *Server) running(w http.ResponseWriter) (node.View, bool) {
	v := s.rt.View()
	if v.Phase != node.PhaseRunning {
		http.Error(w, "site is not ready: "+v.Phase, http.StatusServiceUnavailable)
		return v, false
	}
	return v, true
}

// ---------- between sites ----------

func (s *Server) apply(w http.ResponseWriter, r *http.Request, caller string) {
	v := s.rt.View()
	var b change.Batch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&b); err != nil {
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.Origin != caller {
		// A site may only deliver its own changes.
		http.Error(w, "origin does not match the authenticated site", http.StatusForbidden)
		return
	}
	// Finish applying even if the sender gives up waiting: the work is kept
	// (applied_seq) and its retry simply skips what is already done.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Hour)
	defer cancel()
	ack := v.Applier.Apply(ctx, b)
	if ack.Error != "" {
		s.log.Warn("apply failed", "origin", b.Origin, "applied", ack.Applied, "err", ack.Error)
	}
	writeJSON(w, ack)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request, _ string) {
	v := s.rt.View()
	s.log.Info("serving snapshot", "remote", r.RemoteAddr)
	ver, _ := strconv.Atoi(r.URL.Query().Get("v"))
	if err := snapshot.Serve(r.Context(), w, v.Store.Pool(), s.cfg, ver >= 2); err != nil {
		// Headers may already be sent; the client detects the missing "end" line.
		s.log.Warn("snapshot failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) members(w http.ResponseWriter, r *http.Request, _ string) {
	v := s.rt.View()
	view, err := cluster.LocalView(r.Context(), v.Store.Pool(), v.Identity.ClusterID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

// ---------- status & dashboard data ----------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.statusData(r.Context(), s.isAdmin(r)))
}

// statusData describes this site. The pairing code is only included for an
// admin: it is what proves an approval came from the intended site, so
// anyone who can read it could approve a fake one.
func (s *Server) statusData(ctx context.Context, admin bool) map[string]any {
	v := s.rt.View()
	out := map[string]any{
		"node_id": s.cfg.NodeID, "time": time.Now(), "phase": v.Phase, "advertise": s.cfg.Advertise,
		"admin_enabled": s.cfg.AdminPassword != "", "version": buildinfo.Version,
	}
	if v.Notice != "" {
		out["notice"] = v.Notice
	}
	if v.Error != "" {
		out["error"] = v.Error
	}
	if v.Pairing != nil {
		if !admin {
			v.Pairing.Code, v.Pairing.Hidden = "", true
		}
		out["pairing"] = v.Pairing
	}
	if v.TLS != nil {
		out["fingerprint"] = v.TLS.Fingerprint
	}
	if v.Identity != nil {
		out["cluster_id"] = v.Identity.ClusterID
		out["sequences"] = config.SequenceConfig{Offset: v.Identity.Offset, Step: v.Identity.Step}
	}
	if v.Phase != node.PhaseRunning {
		return out
	}

	out["capture"] = v.Capture.Status()
	ob, err := v.Store.OutboxStats(ctx)
	if err != nil {
		out["error"] = err.Error()
	}
	peers := v.Manager.Statuses()
	for i := range peers {
		peers[i].Backlog = max(0, ob.MaxSeq-peers[i].AckedSeq)
		ob.Pending = max(ob.Pending, peers[i].Backlog)
	}
	out["outbox"] = ob
	out["peers"] = peers
	out["removed"] = v.Manager.Removed()
	if inbox, err := v.Store.InboxStates(ctx); err == nil {
		out["inbox"] = inbox
	}
	if c, err := v.Store.Conflicts(ctx, 10); err == nil {
		out["conflicts"] = c
	}
	if ms, err := cluster.Members(ctx, v.Store.Pool()); err == nil {
		out["members"] = ms
		seen := map[int64]string{}
		for _, m := range ms {
			if m.Status != "active" {
				continue
			}
			if other, dup := seen[m.Offset]; dup {
				out["warning"] = "ไซต์ " + other + " กับ " + m.ID + " ได้เลข ID ชุดเดียวกัน — ถอดไซต์หนึ่งออกแล้วเชิญเข้ามาใหม่"
			}
			seen[m.Offset] = m.ID
		}
	}
	owners := map[string]string{}
	for name, p := range s.cfg.TablesCopy() {
		if p.Owner != "" {
			owners[name] = p.Owner
		}
	}
	out["owners"] = owners
	if wal, err := v.Store.WALStatus(ctx, s.cfg.Capture.Slot); err == nil {
		out["wal"] = wal
		if wal.RetainedBytes > walWarnBytes || wal.SlotStatus == "unreserved" || wal.SlotStatus == "lost" {
			out["wal_warning"] = fmt.Sprintf("Postgres เก็บ WAL ค้างไว้ให้ Conduit %.1f GB (สถานะ %s) — ถ้าปล่อยไว้ดิสก์อาจเต็ม: ตรวจว่าทุกไซต์ซิงก์ได้ และพิจารณาตั้ง max_slot_wal_keep_size",
				float64(wal.RetainedBytes)/1e9, wal.SlotStatus)
		}
	}
	if n, err := v.Store.SkippedSchemaChanges(ctx); err == nil && n > 0 {
		out["schema_skipped"] = n
	}
	out["join_requests"] = s.rt.Requests.Pending()
	return out
}

type meshNode struct {
	ID        string         `json:"id"`
	Self      bool           `json:"self"`
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error,omitempty"`
	Status    map[string]any `json:"status,omitempty"`
}

// mesh returns this site's status plus every active member's, fetched in
// parallel over the pinned peer channel, so the dashboard can draw the whole
// network from any one site.
func (s *Server) mesh(w http.ResponseWriter, r *http.Request) {
	nodes := []meshNode{{ID: s.cfg.NodeID, Self: true, Reachable: true, Status: s.statusData(r.Context(), s.isAdmin(r))}}
	var targets []peer.Target
	v := s.rt.View()
	if v.Phase == node.PhaseRunning {
		ms, _ := cluster.Members(r.Context(), v.Store.Pool())
		for _, m := range ms {
			if m.ID != s.cfg.NodeID && m.Status == "active" {
				nodes = append(nodes, meshNode{ID: m.ID})
				targets = append(targets, peer.Target{URL: m.URL, Fingerprint: m.CertFP})
			}
		}
	}
	creds := s.rt.Credentials()
	var wg sync.WaitGroup
	for i := 1; i < len(nodes); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := &nodes[i]
			resp, err := peer.Do(r.Context(), targets[i-1], creds, http.MethodGet, "/status", nil, 2*time.Second)
			if err == nil && resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				err = errors.New(resp.Status)
			}
			if err == nil {
				var st map[string]any
				err = json.NewDecoder(resp.Body).Decode(&st)
				resp.Body.Close()
				if err == nil {
					n.Status = st
				}
			}
			if err != nil {
				n.Error = err.Error()
			} else {
				n.Reachable = true
			}
		}()
	}
	wg.Wait()
	writeJSON(w, map[string]any{"self": s.cfg.NodeID, "time": time.Now(), "nodes": nodes})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
