// Command receiver runs the standalone Dial MX receiver: the SMTP edge and the
// HTTPS/2 dialer session endpoint in one process. A failure in either listener
// stops both, and shutdown is bounded so a wedged peer cannot hang the process.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dellarb/mailmoose/dialmx"
	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

func main() {
	cfg, err := dialmx.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
	if err != nil {
		slog.Error("load TLS certificate", "error", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r := receiver.New(cfg.Receiver, slog.Default())

	// The SMTP edge hands each message to the in-process receiver's Delivery.
	// The receiver owns domain registration, so the edge is policy-free.
	edge := mxagent.NewServerWithHandoff(cfg.SMTP, slog.Default(), func() mxagent.Delivery {
		return r.NewDelivery()
	})

	// The session listener serves the HTTP/2 dialer protocol over TLS. Only the
	// header read is bounded globally; the session itself relies on per-read
	// deadlines and context cancellation so a long-lived dialer is not killed
	// by a blanket server read timeout.
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}},
	}

	var wg sync.WaitGroup
	run := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil && ctx.Err() == nil {
				slog.Error(name+" failed", "error", err)
				stop()
			}
		}()
	}

	smtpLn, err := net.Listen("tcp", cfg.SMTP.ListenAddr)
	if err != nil {
		slog.Error("listen SMTP", "error", err)
		os.Exit(1)
	}
	run("mx edge", func() error { return edge.ListenAndServe(ctx, smtpLn) })

	tlsLn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		slog.Error("listen session", "error", err)
		os.Exit(1)
	}
	run("session", func() error { return srv.ServeTLS(tlsLn, "", "") })

	<-ctx.Done()
	// Either listener failing calls stop via the run wrapper, so shutdown is
	// shared. Bound it so a stuck peer cannot hold the process open.
	r.Stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
		_ = srv.Close()
	}
}
