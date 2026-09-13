// Command mx is the optional policy-free SMTP edge for Gatehouse Mail. It is
// built from the same module and image as the application and runs as a
// separate process, either as a standalone sidecar/remote edge or as a child of
// the embedded single-container mode. See internal/mxagent and decision D031.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"gatehouse-mail/internal/logging"
	"gatehouse-mail/internal/mxagent"
)

func main() {
	log := logging.New(os.Stderr, slog.LevelInfo, logging.PrefixMX)
	cfg, err := mxagent.Load()
	if err != nil {
		log.Error("invalid mx configuration", "error", err)
		os.Exit(2)
	}
	// Staging is in-memory, so the edge needs no writable filesystem and does
	// not require any privilege. It never runs as root and holds no /data.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}
	srv := mxagent.NewServer(cfg, log)

	// The embedded parent (different uid) cannot signal this process, so it
	// asks for shutdown by closing an inherited pipe; standalone edges use
	// signals.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, stopFD := mxagent.WatchShutdownFD(ctx)
	defer stopFD()

	if cfg.HealthAddr != "" {
		go func() {
			if err := srv.ServeHealth(ctx, cfg.HealthAddr); err != nil {
				log.Warn("mx health listener stopped", "error", err)
			}
		}()
	}
	if err := srv.ListenAndServe(ctx, ln); err != nil && ctx.Err() == nil {
		log.Error("mx edge stopped", "error", err)
		os.Exit(1)
	}
	log.Info("mx edge shutting down", "stats", srv.Stats())
}
