package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-agent-inbox/open-agent-inbox/internal/app"
	"github.com/open-agent-inbox/open-agent-inbox/internal/config"
	"github.com/open-agent-inbox/open-agent-inbox/internal/events"
	"github.com/open-agent-inbox/open-agent-inbox/internal/httpapp"
	"github.com/open-agent-inbox/open-agent-inbox/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		os.Exit(2)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Error("database open failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()
	hub := events.NewHub()
	svc, err := app.New(cfg, st, hub)
	if err != nil {
		log.Error("application init failed", "error", err)
		os.Exit(1)
	}
	h := httpapp.New(svc, log).Handler()
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: h, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	go func() {
		log.Info("Open Agent Inbox listening", "addr", cfg.ListenAddr, "mode", cfg.Mode, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
