// Command mx is the optional policy-free SMTP edge for Gatehouse Mail. It is
// built from the same module and image as the application but runs as a
// separate, non-root process. See internal/mxagent and decision D031.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"gatehouse-mail/internal/mxagent"
	"gatehouse-mail/internal/privdrop"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := mxagent.Load()
	if err != nil {
		log.Error("invalid mx configuration", "error", err)
		os.Exit(2)
	}
	// The image has no USER, so the edge boots as root only to fix up its
	// staging directory for the runtime user and then shed privileges before
	// serving. When the process is already non-root this is a no-op.
	dropped, runUID, runGID, err := privdrop.DropToRuntimeUser(cfg.StagingDir)
	if err != nil {
		log.Error("privilege drop failed", "error", err)
		os.Exit(1)
	}
	log.Info("runtime identity", "uid", runUID, "gid", runGID, "dropped", dropped)
	if err := mxagent.EnsureStaging(cfg.StagingDir); err != nil {
		log.Error("cannot prepare staging directory", "error", err)
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("cannot listen", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}
	srv := mxagent.NewServer(cfg, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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
