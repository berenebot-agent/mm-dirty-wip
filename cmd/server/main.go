package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/store"
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
	h := httpapp.New(svc, log)

	type listener struct {
		name string
		srv  *http.Server
	}
	start := func(name, addr string, handler http.Handler) (*listener, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("%s listener: %w", name, err)
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
		go func() {
			log.Info("Open Agent Inbox listening", "listener", name, "addr", ln.Addr().String(), "mode", cfg.Mode, "base_url", cfg.BaseURL)
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Error("HTTP server failed", "listener", name, "error", err)
				os.Exit(1)
			}
		}()
		return &listener{name: name, srv: srv}, nil
	}

	mainListener, err := start("main", cfg.ListenAddr, h.Handler())
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	listeners := []*listener{mainListener}
	if cfg.InboundListenAddr != "" {
		inboundListener, err := start("inbound", cfg.InboundListenAddr, h.InboundHandler())
		if err != nil {
			log.Error("startup failed", "error", err)
			os.Exit(1)
		}
		listeners = append(listeners, inboundListener)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, l := range listeners {
		if err := l.srv.Shutdown(ctx); err != nil {
			log.Warn("HTTP shutdown failed", "listener", l.name, "error", err)
		}
	}
}
