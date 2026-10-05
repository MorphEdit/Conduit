// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

// Command conduit keeps Postgres databases on several sites in sync.
//
//	conduit              run the site (dashboard on :7420, peers on :7443)
//	conduit invite       print a one-time invite code for a new site
//	conduit cleanup      remove everything Conduit added to this database
//	conduit version      print the version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MorphEdit/conduit/internal/buildinfo"
	"github.com/MorphEdit/conduit/internal/cluster"
	"github.com/MorphEdit/conduit/internal/config"
	"github.com/MorphEdit/conduit/internal/node"
	"github.com/MorphEdit/conduit/internal/server"
)

func main() {
	cfgPath := flag.String("config", "/etc/conduit/conduit.yaml", "optional config file (CONDUIT_* env vars override it)")
	flag.Parse()
	if flag.Arg(0) == "version" {
		fmt.Println(buildinfo.String())
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if limit := containerMemoryLimit(); limit > 0 {
		// Go does not know about a container's memory cap; without this the
		// heap may grow past it before collecting and the process is killed.
		debug.SetMemoryLimit(limit * 3 / 4)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("invalid config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch flag.Arg(0) {
	case "":
		err = run(ctx, cfg, log.With("node", cfg.NodeID))
	case "invite":
		err = invite(ctx, cfg)
	case "cleanup":
		err = cleanup(ctx, cfg, flag.Arg(1) == "--yes")
	default:
		err = fmt.Errorf("unknown command %q (commands: invite, cleanup, version)", flag.Arg(0))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// containerMemoryLimit reads the cgroup memory cap (v2, then v1); 0 if none.
func containerMemoryLimit() int64 {
	for _, f := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err == nil && n > 0 && n < 1<<50 {
			return n
		}
	}
	return 0
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	rt := node.New(cfg, log)
	srv := server.New(rt)
	rt.PeerHandler = srv.PeerHandler()
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.DashboardHandler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()
	log.Info(buildinfo.String())
	log.Info("conduit started", "dashboard", cfg.Listen, "peer_port", cfg.PeerListen, "advertise", cfg.Advertise)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- rt.Run(ctx) }()

	select {
	case err := <-serveErr:
		cancel()
		<-runErr
		return err
	case err := <-runErr:
		shutdown, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		httpSrv.Shutdown(shutdown)
		return err
	}
}

func invite(ctx context.Context, cfg *config.Config) error {
	pool, err := pgxpool.New(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	id, err := cluster.LoadIdentity(ctx, pool)
	if err != nil || id == nil || !id.Ready {
		return errors.New("this site has not joined a cluster yet")
	}
	tlsm, err := cluster.LoadOrCreateTLS(ctx, pool, id.ID)
	if err != nil {
		return err
	}
	code, exp, err := cluster.CreateInvite(ctx, pool, cfg.Advertise, tlsm.Fingerprint, 24*time.Hour)
	if err != nil {
		return err
	}
	fmt.Println(code)
	fmt.Fprintf(os.Stderr, "valid until %s, single use. On the new site set CONDUIT_JOIN=<code> or paste it on its dashboard.\n", exp.Format(time.RFC1123))
	return nil
}

// cleanup removes Conduit from this database: the replication slot (which
// would otherwise keep WAL forever), the publication, the id-alignment event
// trigger and the conduit schema. Synced data in your tables is left alone.
// Remove the site on another site's dashboard first.
func cleanup(ctx context.Context, cfg *config.Config, yes bool) error {
	if !yes {
		fmt.Println("This removes Conduit's replication slot, publication, event trigger and the \"conduit\" schema")
		fmt.Println("from", cfg.Database[:min(len(cfg.Database), 12)]+"…  Your own tables and data are not touched.")
		fmt.Println("Stop the Conduit service first, then run:  conduit cleanup --yes")
		return nil
	}
	pool, err := pgxpool.New(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	steps := []string{
		`SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = '` + cfg.Capture.Slot + `'`,
		`DROP PUBLICATION IF EXISTS ` + cfg.Capture.Publication,
		`DROP EVENT TRIGGER IF EXISTS conduit_align_ids`,
		`DROP SCHEMA IF EXISTS conduit CASCADE`,
	}
	for _, q := range steps {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w (is Conduit still running?)", q, err)
		}
	}
	fmt.Println("Conduit removed from this database (owner-only guards went with it). Id sequences keep their step size.")
	return nil
}
