// Package server exposes the peer apply endpoint plus health and status.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/conduit-sync/conduit/internal/apply"
	"github.com/conduit-sync/conduit/internal/capture"
	"github.com/conduit-sync/conduit/internal/change"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/sender"
	"github.com/conduit-sync/conduit/internal/store"
)

const maxBody = 64 << 20

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
	return mux
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	want := []byte("Bearer " + s.cfg.Token)
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
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
	out := map[string]any{"node_id": s.cfg.NodeID, "time": time.Now()}
	if s.capture != nil {
		out["capture"] = s.capture.Status()
	} else {
		out["capture"] = capture.Status{Enabled: false}
	}
	ob, err := s.store.OutboxStats(r.Context())
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
	if inbox, err := s.store.InboxStates(r.Context()); err == nil {
		out["inbox"] = inbox
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
