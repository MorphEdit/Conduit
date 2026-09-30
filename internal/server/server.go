// Package server is Conduit's HTTP surface: peer sync, cluster membership,
// admin actions and the dashboard.
package server

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/conduit-sync/conduit/internal/change"
	"github.com/conduit-sync/conduit/internal/cluster"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/node"
	"github.com/conduit-sync/conduit/internal/snapshot"
)

const maxBody = 64 << 20

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

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /v1/mesh", s.mesh)

	// Between sites (cluster token).
	mux.HandleFunc("POST /v1/apply", s.apply)
	mux.HandleFunc("GET /v1/snapshot", s.snapshot)
	mux.HandleFunc("GET /v1/members", s.members)
	mux.HandleFunc("GET /v1/schema", s.schema)

	// New sites asking to join (invite secret / request secret).
	mux.HandleFunc("POST /v1/join", s.join)
	mux.HandleFunc("POST /v1/join-requests", s.joinRequest)
	mux.HandleFunc("GET /v1/join-requests/{id}", s.joinPoll)

	// Dashboard actions (admin password).
	mux.HandleFunc("GET /v1/admin/check", s.admin(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	mux.HandleFunc("POST /v1/admin/invites", s.admin(s.createInvite))
	mux.HandleFunc("POST /v1/admin/requests/{id}/{decision}", s.admin(s.decide))
	mux.HandleFunc("POST /v1/admin/members/{id}/remove", s.admin(s.removeMember))
	mux.HandleFunc("POST /v1/admin/join", s.admin(s.pasteInvite))

	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	return mux
}

// ---------- auth ----------

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// clusterAuth checks the shared cluster token (known once this site joined).
func (s *Server) clusterAuth(w http.ResponseWriter, r *http.Request) bool {
	token := s.cfg.Token
	if s.rt.View().Identity == nil || token == "" || !equal(r.Header.Get("Authorization"), "Bearer "+token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
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

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAuth(w, r) {
		return
	}
	v, ok := s.running(w)
	if !ok {
		return
	}
	var b change.Batch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&b); err != nil {
		http.Error(w, "bad batch: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !config.ValidID(b.Origin) || b.Origin == s.cfg.NodeID {
		http.Error(w, "invalid origin", http.StatusBadRequest)
		return
	}
	ack := v.Applier.Apply(r.Context(), b)
	if ack.Error != "" {
		s.log.Warn("apply failed", "origin", b.Origin, "applied", ack.Applied, "err", ack.Error)
	}
	writeJSON(w, ack)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAuth(w, r) {
		return
	}
	v, ok := s.running(w)
	if !ok {
		return
	}
	s.log.Info("serving snapshot", "remote", r.RemoteAddr)
	if err := snapshot.Serve(r.Context(), w, v.Store.Pool(), s.cfg); err != nil {
		// Headers may already be sent; the client detects the missing "end" line.
		s.log.Warn("snapshot failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	if !s.clusterAuth(w, r) {
		return
	}
	v, ok := s.running(w)
	if !ok {
		return
	}
	view, err := cluster.LocalView(r.Context(), v.Store.Pool(), v.Identity.ClusterID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, view)
}

// ---------- status & dashboard data ----------

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.statusData(r.Context()))
}

func (s *Server) statusData(ctx context.Context) map[string]any {
	v := s.rt.View()
	out := map[string]any{
		"node_id": s.cfg.NodeID, "time": time.Now(), "phase": v.Phase, "advertise": s.cfg.Advertise,
		"admin_enabled": s.cfg.AdminPassword != "",
	}
	if v.Notice != "" {
		out["notice"] = v.Notice
	}
	if v.Error != "" {
		out["error"] = v.Error
	}
	if v.Pairing != nil {
		out["pairing"] = v.Pairing
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
// parallel, so the dashboard can draw the whole network from any one site.
func (s *Server) mesh(w http.ResponseWriter, r *http.Request) {
	nodes := []meshNode{{ID: s.cfg.NodeID, Self: true, Reachable: true, Status: s.statusData(r.Context())}}
	v := s.rt.View()
	if v.Phase == node.PhaseRunning {
		ms, _ := cluster.Members(r.Context(), v.Store.Pool())
		for _, m := range ms {
			if m.ID != s.cfg.NodeID && m.Status == "active" {
				nodes = append(nodes, meshNode{ID: m.ID, Status: map[string]any{"advertise": m.URL}})
			}
		}
	}
	var wg sync.WaitGroup
	for i := 1; i < len(nodes); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := &nodes[i]
			url, _ := n.Status["advertise"].(string)
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/status", nil)
			resp, err := http.DefaultClient.Do(req)
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
