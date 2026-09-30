// Package server exposes the peer apply endpoint plus health and status.
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

	"github.com/conduit-sync/conduit/internal/apply"
	"github.com/conduit-sync/conduit/internal/capture"
	"github.com/conduit-sync/conduit/internal/change"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/sender"
	"github.com/conduit-sync/conduit/internal/snapshot"
	"github.com/conduit-sync/conduit/internal/store"
)

const maxBody = 64 << 20

// The dashboard served at "/".
//
//go:embed ui
var uiFiles embed.FS

type Server struct {
	cfg     *config.Config
	store   *store.Store
	capture *capture.Capture // nil when capture is disabled
	senders []*sender.Sender
	applier *apply.Applier
	log     *slog.Logger
}

func New(cfg *config.Config, st *store.Store, c *capture.Capture, senders []*sender.Sender,
	a *apply.Applier, log *slog.Logger) *Server {
	return &Server{cfg: cfg, store: st, capture: c, senders: senders, applier: a, log: log.With("component", "server")}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("POST /v1/apply", s.apply)
	mux.HandleFunc("GET /v1/snapshot", s.snapshot)
	mux.HandleFunc("GET /v1/mesh", s.mesh)
	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	return mux
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	want := []byte("Bearer " + s.cfg.Token)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	s.log.Info("serving snapshot", "remote", r.RemoteAddr)
	if err := snapshot.Serve(r.Context(), w, s.store.Pool(), s.cfg); err != nil {
		// Headers may already be sent; the client detects the missing "end" line.
		s.log.Warn("snapshot failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
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
	ack := s.applier.Apply(r.Context(), b)
	if ack.Error != "" {
		s.log.Warn("apply failed", "origin", b.Origin, "applied", ack.Applied, "err", ack.Error)
	}
	writeJSON(w, ack)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.statusData(r.Context()))
}

func (s *Server) statusData(ctx context.Context) map[string]any {
	out := map[string]any{"node_id": s.cfg.NodeID, "time": time.Now(), "sequences": s.cfg.Sequences}
	if s.capture != nil {
		out["capture"] = s.capture.Status()
	} else {
		out["capture"] = capture.Status{Enabled: false}
	}
	ob, err := s.store.OutboxStats(ctx)
	if err != nil {
		out["error"] = err.Error()
	}
	out["outbox"] = ob
	peers := make([]sender.Status, len(s.senders))
	for i, snd := range s.senders {
		p := snd.Status()
		p.Backlog = max(0, ob.MaxSeq-p.AckedSeq)
		peers[i] = p
	}
	out["peers"] = peers
	if inbox, err := s.store.InboxStates(ctx); err == nil {
		out["inbox"] = inbox
	}
	if c, err := s.store.Conflicts(ctx, 10); err == nil {
		out["conflicts"] = c
	}
	owners := map[string]string{}
	for name, p := range s.cfg.Tables {
		if p.Owner != "" {
			owners[name] = p.Owner
		}
	}
	out["owners"] = owners
	return out
}

type meshNode struct {
	ID        string         `json:"id"`
	Self      bool           `json:"self"`
	Reachable bool           `json:"reachable"`
	Error     string         `json:"error,omitempty"`
	Status    map[string]any `json:"status,omitempty"`
}

// mesh returns this node's status plus every peer's, fetched in parallel,
// so the dashboard can draw the whole network from any one site.
func (s *Server) mesh(w http.ResponseWriter, r *http.Request) {
	nodes := make([]meshNode, len(s.cfg.Peers)+1)
	nodes[0] = meshNode{ID: s.cfg.NodeID, Self: true, Reachable: true, Status: s.statusData(r.Context())}
	var wg sync.WaitGroup
	for i, p := range s.cfg.Peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := meshNode{ID: p.ID}
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.URL, "/")+"/status", nil)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				err = json.NewDecoder(resp.Body).Decode(&n.Status)
				resp.Body.Close()
			}
			if err != nil {
				n.Error = err.Error()
			} else {
				n.Reachable = true
			}
			nodes[i+1] = n
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
