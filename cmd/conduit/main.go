// Command conduit keeps Postgres databases on several sites in sync.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/apply"
	"github.com/conduit-sync/conduit/internal/capture"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/notify"
	"github.com/conduit-sync/conduit/internal/policy"
	"github.com/conduit-sync/conduit/internal/sender"
	"github.com/conduit-sync/conduit/internal/server"
	"github.com/conduit-sync/conduit/internal/snapshot"
	"github.com/conduit-sync/conduit/internal/store"
)

func main() {
	cfgPath := flag.String("config", "conduit.yaml", "path to config file")
	snapshotFrom := flag.String("snapshot-from", "", "copy all tables from this peer, then exit (stop the service first)")
	truncate := flag.Bool("truncate", false, "with -snapshot-from: empty local tables before loading")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(1)
	}
	log = log.With("node", cfg.NodeID)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *snapshotFrom != "" {
		err = runSnapshot(ctx, cfg, *snapshotFrom, *truncate, log)
	} else {
		err = run(ctx, cfg, log)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// open connects and bootstraps Conduit's schema, waiting for the database.
func open(ctx context.Context, cfg *config.Config, log *slog.Logger) (*store.Store, error) {
	pool, err := pgxpool.New(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	st := store.New(pool)
	for attempt := 1; ; attempt++ {
		if err = st.Bootstrap(ctx); err == nil {
			return st, nil
		}
		log.Warn("database not ready", "err", err, "attempt", attempt)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func runSnapshot(ctx context.Context, cfg *config.Config, peer string, truncate bool, log *slog.Logger) error {
	st, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Pool().Close()
	ap := apply.New(cfg, log)
	defer ap.Close()
	if err := snapshot.Pull(ctx, cfg, peer, truncate, ap, log); err != nil {
		return err
	}
	return policy.Setup(ctx, st.Pool(), cfg, log)
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	st, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer st.Pool().Close()
	if err := st.EnsureCursors(ctx, cfg.PeerIDs()); err != nil {
		return err
	}
	if err := policy.Setup(ctx, st.Pool(), cfg, log); err != nil {
		return err
	}

	var (
		wg   sync.WaitGroup
		bus  notify.Bus
		capt *capture.Capture
	)
	if cfg.Capture.Enabled {
		capt = capture.New(cfg, st, &bus, log)
		if err := capt.Setup(ctx); err != nil {
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); capt.Run(ctx) }()
	}

	senders := make([]*sender.Sender, len(cfg.Peers))
	for i, p := range cfg.Peers {
		s := sender.New(cfg, p, st, bus.Subscribe(), log)
		senders[i] = s
		wg.Add(1)
		go func() { defer wg.Done(); s.Run(ctx) }()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if err := st.Janitor(ctx, cfg.TombstoneTTL); err != nil && ctx.Err() == nil {
				log.Warn("janitor", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Hour):
			}
		}
	}()

	applier := apply.New(cfg, log)
	defer applier.Close()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(cfg, st, capt, senders, applier, log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdown)
	}()

	log.Info("conduit started", "listen", cfg.Listen, "capture", cfg.Capture.Enabled, "peers", cfg.PeerIDs())
	err = httpSrv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	cancel()
	wg.Wait()
	return err
}
