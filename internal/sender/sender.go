// Package sender ships outbox transactions to one peer and advances that
// peer's cursor when it acknowledges them.
package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/conduit-sync/conduit/internal/change"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/peer"
	"github.com/conduit-sync/conduit/internal/store"
)

const (
	batchSize    = 200
	idlePoll     = 5 * time.Second
	maxBackoff   = 30 * time.Second
	requestLimit = 30 * time.Second
)

type Status struct {
	ID        string     `json:"id"`
	URL       string     `json:"url"`
	AckedSeq  int64      `json:"acked_seq"`
	Backlog   int64      `json:"backlog"`
	LastOK    *time.Time `json:"last_ok,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// Peer is a site this node ships its changes to.
type Peer struct {
	ID  string
	URL string
	FP  string // pinned certificate fingerprint
}

type Sender struct {
	peer      Peer
	nodeID    string
	creds     peer.Credentials
	retention time.Duration
	store     *store.Store
	wake      <-chan struct{}
	log       *slog.Logger

	mu sync.Mutex
	st Status
}

func New(cfg *config.Config, p Peer, creds peer.Credentials, st *store.Store, wake <-chan struct{}, log *slog.Logger) *Sender {
	return &Sender{
		peer:      p,
		nodeID:    cfg.NodeID,
		creds:     creds,
		retention: cfg.OutboxRetention,
		store:     st,
		wake:      wake,
		log:       log.With("component", "sender", "peer", p.ID),
		st:        Status{ID: p.ID, URL: p.URL},
	}
}

// Wake returns the channel this sender listens on (for unsubscribing).
func (s *Sender) Wake() <-chan struct{} { return s.wake }

func (s *Sender) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *Sender) Run(ctx context.Context) {
	backoff := time.Second
	fail := func(err error) {
		s.mu.Lock()
		s.st.LastError = err.Error()
		s.mu.Unlock()
		s.log.Warn("send failed; will retry", "err", err, "in", backoff)
		sleep(ctx, backoff)
		backoff = min(backoff*2, maxBackoff)
	}

	cursor, err := s.store.Cursor(ctx, s.peer.ID)
	for err != nil && ctx.Err() == nil {
		fail(err)
		cursor, err = s.store.Cursor(ctx, s.peer.ID)
	}
	s.setAcked(cursor)

	for ctx.Err() == nil {
		txs, err := s.store.ReadOutbox(ctx, cursor, batchSize)
		if err != nil {
			fail(err)
			continue
		}
		if len(txs) == 0 {
			select {
			case <-ctx.Done():
			case <-s.wake:
			case <-time.After(idlePoll):
			}
			continue
		}

		ack, err := s.send(ctx, txs)
		if ack.Applied > cursor {
			cursor = ack.Applied
			if err := s.store.SetCursor(ctx, s.peer.ID, cursor); err != nil {
				s.log.Warn("save cursor", "err", err)
			}
			if err := s.store.Trim(ctx, s.nodeID, s.retention); err != nil {
				s.log.Warn("trim outbox", "err", err)
			}
			s.setAcked(cursor)
		}
		if err == nil && ack.Error != "" {
			err = fmt.Errorf("peer could not apply: %s", ack.Error)
		}
		if err != nil {
			fail(err)
			continue
		}
		backoff = time.Second
		now := time.Now()
		s.mu.Lock()
		s.st.LastOK, s.st.LastError = &now, ""
		s.mu.Unlock()
		s.log.Info("delivered", "txs", len(txs), "acked_seq", cursor)
	}
}

func (s *Sender) send(ctx context.Context, txs []change.Tx) (change.Ack, error) {
	body, err := json.Marshal(change.Batch{Origin: s.nodeID, Txs: txs})
	if err != nil {
		return change.Ack{}, err
	}
	resp, err := peer.Do(ctx, peer.Target{URL: s.peer.URL, Fingerprint: s.peer.FP}, s.creds,
		http.MethodPost, "/v1/apply", bytes.NewReader(body), requestLimit)
	if err != nil {
		return change.Ack{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return change.Ack{}, fmt.Errorf("peer returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var ack change.Ack
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return change.Ack{}, errors.Join(errors.New("bad ack"), err)
	}
	return ack, nil
}

func (s *Sender) setAcked(seq int64) {
	s.mu.Lock()
	s.st.AckedSeq = seq
	s.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
