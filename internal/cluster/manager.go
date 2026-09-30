package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/notify"
	"github.com/conduit-sync/conduit/internal/sender"
	"github.com/conduit-sync/conduit/internal/store"
)

const gossipInterval = 10 * time.Second

// Manager keeps the member list in step with the other sites and runs one
// sender per active member, starting and stopping them as sites come and go.
type Manager struct {
	cfg        *config.Config
	id         *Identity
	st         *store.Store
	bus        *notify.Bus
	log        *slog.Logger
	onSettings func(context.Context)
	client     *http.Client
	trigger    chan struct{}

	mu      sync.Mutex
	senders map[string]*running
	removed bool
}

type running struct {
	s      *sender.Sender
	url    string
	cancel context.CancelFunc
}

func NewManager(cfg *config.Config, id *Identity, st *store.Store, bus *notify.Bus, onSettings func(context.Context), log *slog.Logger) *Manager {
	return &Manager{
		cfg: cfg, id: id, st: st, bus: bus, onSettings: onSettings,
		log:     log.With("component", "cluster"),
		client:  &http.Client{Timeout: 3 * time.Second},
		trigger: make(chan struct{}, 1),
		senders: map[string]*running{},
	}
}

// Trigger runs a gossip round soon (e.g. right after admitting a site).
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

func (m *Manager) Run(ctx context.Context) {
	m.reconcile(ctx)
	t := time.NewTicker(gossipInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.stopAll()
			return
		case <-t.C:
		case <-m.trigger:
		}
		m.round(ctx)
	}
}

func (m *Manager) round(ctx context.Context) {
	pool := m.st.Pool()
	members, err := Members(ctx, pool)
	if err != nil {
		m.log.Warn("load members", "err", err)
		return
	}
	changed := false
	for _, mem := range members {
		if mem.ID == m.id.ID || mem.Status != "active" {
			continue
		}
		v, err := m.fetch(ctx, mem.URL)
		if err != nil {
			continue // unreachable sites are the sender's business
		}
		if v.ClusterID != m.id.ClusterID {
			m.log.Warn("member belongs to another cluster; ignoring", "member", mem.ID, "their_cluster", v.ClusterID)
			continue
		}
		c, err := Merge(ctx, pool, v)
		if err != nil {
			m.log.Warn("merge view", "from", mem.ID, "err", err)
		}
		changed = changed || c
	}
	if changed {
		m.log.Info("cluster view changed")
		if m.onSettings != nil {
			m.onSettings(ctx)
		}
	}
	m.reconcile(ctx)
}

func (m *Manager) fetch(ctx context.Context, url string) (View, error) {
	var v View
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/members", nil)
	if err != nil {
		return v, err
	}
	req.Header.Set("Authorization", "Bearer "+m.id.Token)
	resp, err := m.client.Do(req)
	if err != nil {
		return v, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return v, fmt.Errorf("status %s", resp.Status)
	}
	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// reconcile starts a sender for each active peer and stops the rest.
func (m *Manager) reconcile(ctx context.Context) {
	members, err := Members(ctx, m.st.Pool())
	if err != nil {
		return
	}
	want := map[string]string{}
	selfRemoved := false
	for _, mem := range members {
		if mem.ID == m.id.ID {
			selfRemoved = mem.Status == "removed"
			continue
		}
		if mem.Status == "active" {
			want[mem.ID] = mem.URL
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if selfRemoved && !m.removed {
		m.log.Warn("this site was removed from the cluster; sync stopped")
	}
	m.removed = selfRemoved
	if selfRemoved {
		want = nil
	}
	for id, r := range m.senders {
		if url, ok := want[id]; !ok || url != r.url {
			r.cancel()
			m.bus.Unsubscribe(r.s.Wake())
			delete(m.senders, id)
			if !ok {
				m.st.DeleteCursor(ctx, id)
				m.log.Info("stopped syncing to removed site", "peer", id)
			}
		}
	}
	for id, url := range want {
		if _, ok := m.senders[id]; ok {
			continue
		}
		if err := m.st.EnsureCursor(ctx, id); err != nil {
			m.log.Warn("cursor", "peer", id, "err", err)
			continue
		}
		sctx, cancel := context.WithCancel(ctx)
		s := sender.New(m.cfg, sender.Peer{ID: id, URL: url}, m.st, m.bus.Subscribe(), m.log)
		m.senders[id] = &running{s: s, url: url, cancel: cancel}
		go s.Run(sctx)
		m.log.Info("syncing with site", "peer", id, "url", url)
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.senders {
		r.cancel()
		delete(m.senders, id)
	}
}

// Statuses reports every running sender, sorted by peer id.
func (m *Manager) Statuses() []sender.Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sender.Status, 0, len(m.senders))
	for _, r := range m.senders {
		out = append(out, r.s.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *Manager) Removed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.removed
}
