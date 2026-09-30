// Package node takes a site from "just started" to "syncing": it waits for
// the database, fixes Postgres settings, joins (or founds) the cluster,
// copies schema and data if needed, then starts every component.
package node

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/apply"
	"github.com/conduit-sync/conduit/internal/capture"
	"github.com/conduit-sync/conduit/internal/cluster"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/notify"
	"github.com/conduit-sync/conduit/internal/policy"
	"github.com/conduit-sync/conduit/internal/store"
)

// Phases shown on the dashboard.
const (
	PhaseStarting     = "starting"
	PhaseWaitingDB    = "waiting_db"
	PhaseNeedsRestart = "needs_restart"
	PhaseWaitingJoin  = "waiting_to_join"
	PhaseJoining      = "joining"
	PhaseRunning      = "running"
)

// Pairing is what a site waiting to join shows on its own dashboard.
type Pairing struct {
	Code   string `json:"code"`              // compare with the code on the approving site
	SeedID string `json:"seed_id,omitempty"` // site the request went to
	Status string `json:"status"`            // searching | requested | rejected
}

// Runtime is the live state shared with the HTTP server.
type Runtime struct {
	Cfg      *config.Config
	Log      *slog.Logger
	Requests *cluster.Requests

	mu       sync.RWMutex
	phase    string
	notice   string
	err      string
	store    *store.Store
	identity *cluster.Identity
	capture  *capture.Capture
	manager  *cluster.Manager
	applier  *apply.Applier
	pairing  *Pairing
	pasted   chan string
}

func New(cfg *config.Config, log *slog.Logger) *Runtime {
	return &Runtime{Cfg: cfg, Log: log, Requests: cluster.NewRequests(), phase: PhaseStarting, pasted: make(chan string, 1)}
}

// View is a consistent snapshot of the runtime for request handlers.
type View struct {
	Phase    string
	Notice   string
	Error    string
	Store    *store.Store
	Identity *cluster.Identity
	Capture  *capture.Capture
	Manager  *cluster.Manager
	Applier  *apply.Applier
	Pairing  *Pairing
}

func (r *Runtime) View() View {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v := View{Phase: r.phase, Notice: r.notice, Error: r.err, Store: r.store, Identity: r.identity,
		Capture: r.capture, Manager: r.manager, Applier: r.applier}
	if r.pairing != nil {
		p := *r.pairing
		v.Pairing = &p
	}
	return v
}

func (r *Runtime) set(f func()) { r.mu.Lock(); f(); r.mu.Unlock() }

func (r *Runtime) setPhase(phase, notice string) {
	r.set(func() { r.phase, r.notice = phase, notice })
	r.Log.Info("phase", "phase", phase, "notice", notice)
}

func (r *Runtime) setErr(err error) {
	r.set(func() {
		if err == nil {
			r.err = ""
		} else {
			r.err = err.Error()
		}
	})
}

// PasteInvite hands an invite code (entered on the dashboard) to a site
// that is waiting to join.
func (r *Runtime) PasteInvite(code string) error {
	if _, err := cluster.DecodeInvite(code); err != nil {
		return err
	}
	if r.View().Phase != PhaseWaitingJoin {
		return errors.New("this site is not waiting to join")
	}
	select {
	case r.pasted <- code:
		return nil
	default:
		return errors.New("an invite is already being processed")
	}
}

// Run blocks until ctx ends. Startup steps retry forever, so the process
// never needs a restart to recover from a missing database or peer.
func (r *Runtime) Run(ctx context.Context) error {
	cfg, log := r.Cfg, r.Log

	pool, err := pgxpool.New(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)

	r.setPhase(PhaseWaitingDB, "")
	if err := retry(ctx, r, func() error { return pool.Ping(ctx) }); err != nil {
		return err
	}
	if err := r.ensurePostgres(ctx, pool); err != nil {
		return err
	}
	if err := retry(ctx, r, func() error { return st.Bootstrap(ctx) }); err != nil {
		return err
	}
	r.set(func() { r.store = st })

	id, err := cluster.LoadIdentity(ctx, pool)
	if err != nil {
		return err
	}
	switch {
	case id != nil && id.Ready:
	case id != nil: // crashed half-way through joining: redo the data copy
		if id, err = r.finishJoin(ctx, st, id, "", true); err != nil {
			return err
		}
	case cfg.Bootstrap:
		if id, err = cluster.Found(ctx, pool, cfg); err != nil {
			return err
		}
		log.Info("founded a new cluster", "cluster", id.ClusterID)
	default:
		if id, err = r.join(ctx, st); err != nil {
			return err
		}
	}
	return r.start(ctx, st, id)
}

// start brings up capture, apply, gossip and discovery on a joined site.
func (r *Runtime) start(ctx context.Context, st *store.Store, id *cluster.Identity) error {
	cfg, log, pool := r.Cfg, r.Log, st.Pool()
	cfg.NodeID, cfg.Token = id.ID, id.Token
	cfg.Sequences = &config.SequenceConfig{Offset: id.Offset, Step: id.Step}
	r.set(func() { r.identity = id })

	if err := cluster.UpdateSelfURL(ctx, pool, id.ID, cfg.Advertise); err != nil {
		return err
	}
	// Table policies: a site whose own config lists them publishes them to
	// the cluster; everyone else adopts the cluster's.
	if len(cfg.TablesCopy()) > 0 {
		if err := cluster.PutSetting(ctx, pool, "tables", cfg.TablesCopy()); err != nil {
			return err
		}
	}
	applySettings := func(ctx context.Context) {
		if t, ok, err := cluster.TablesSetting(ctx, pool); err == nil && ok {
			cfg.SetTables(t)
		}
		if err := policy.Setup(ctx, pool, cfg, log); err != nil {
			log.Warn("policy", "err", err)
		}
	}
	applySettings(ctx)

	var bus notify.Bus
	capt := capture.New(cfg, st, &bus, log)
	if err := retry(ctx, r, func() error { return capt.Setup(ctx) }); err != nil {
		return err
	}
	applier := apply.New(cfg, log)
	defer applier.Close()
	mgr := cluster.NewManager(cfg, id, st, &bus, applySettings, log)

	var wg sync.WaitGroup
	goRun := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	goRun(func() { capt.Run(ctx) })
	goRun(func() { mgr.Run(ctx) })
	goRun(func() {
		for {
			if err := st.Janitor(ctx, cfg.TombstoneTTL); err != nil && ctx.Err() == nil {
				log.Warn("janitor", "err", err)
			}
			st.Trim(ctx, id.ID, cfg.OutboxRetention)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Minute):
			}
		}
	})
	if cfg.Discovery {
		goRun(func() {
			cluster.Broadcast(ctx, cluster.Beacon{Cluster: id.ClusterID, ID: id.ID, URL: cfg.Advertise, Offset: id.Offset}, log)
		})
	}

	r.set(func() { r.capture, r.manager, r.applier, r.pairing, r.err = capt, mgr, applier, nil, "" })
	r.setPhase(PhaseRunning, "")
	log.Info("site is syncing", "id", id.ID, "offset", id.Offset, "step", id.Step)
	wg.Wait()
	return ctx.Err()
}

// retry runs f until it succeeds, showing the last error on the dashboard.
func retry(ctx context.Context, r *Runtime, f func() error) error {
	delay := time.Second
	for {
		err := f()
		r.setErr(err)
		if err == nil {
			return nil
		}
		r.Log.Warn("retrying", "phase", r.View().Phase, "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 15*time.Second)
	}
}

func pairingCode() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("%03d %03d", n.Int64()/1000, n.Int64()%1000)
}
