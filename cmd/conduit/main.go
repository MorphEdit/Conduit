// Command conduit keeps Postgres databases on several sites in sync.
//
//	conduit              run the site (dashboard on :7420)
//	conduit invite       print a one-time invite code for a new site
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
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/conduit-sync/conduit/internal/cluster"
	"github.com/conduit-sync/conduit/internal/config"
	"github.com/conduit-sync/conduit/internal/node"
	"github.com/conduit-sync/conduit/internal/server"
)

func main() {
	cfgPath := flag.String("config", "/etc/conduit/conduit.yaml", "optional config file (CONDUIT_* env vars override it)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
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
	default:
		err = fmt.Errorf("unknown command %q (commands: invite)", flag.Arg(0))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	rt := node.New(cfg, log)
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: server.New(rt).Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()
	log.Info("conduit started", "listen", cfg.Listen, "advertise", cfg.Advertise)

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
	code, exp, err := cluster.CreateInvite(ctx, pool, cfg.Advertise, 24*time.Hour)
	if err != nil {
		return err
	}
	fmt.Println(code)
	fmt.Fprintf(os.Stderr, "valid until %s, single use. On the new site set CONDUIT_JOIN=<code> or paste it on its dashboard.\n", exp.Format(time.RFC1123))
	return nil
}
