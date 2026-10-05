// Command receiver runs the Dial MX receiver. It has two entry paths:
//
//   - standalone (default): the SMTP edge and the HTTPS/2 dialer session
//     endpoint in one process, configured entirely from the environment. This
//     is the standalone image and the shared/remote deployment. A failure in
//     either listener stops both, and shutdown is bounded.
//   - included standby: when the core spawns this binary with the inherited
//     DIALMX_CONTROL_CMD_FD/DIALMX_CONTROL_REPLY_FD descriptors, the process
//     binds nothing until the core activates it over the private control
//     channel. It then serves the same SMTP edge and session endpoint against
//     fixed endpoints and can be deactivated and reactivated without exiting.
//
// Every log line is a JSON record carrying schema_version, service and boot_id.
// The boot id is generated once here and injected into the one logger shared by
// the receiver and the SMTP edge, so all process events correlate.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dellarb/mailmoose/dialmx"
	"github.com/dellarb/mailmoose/dialmx/control"
	"github.com/dellarb/mailmoose/dialmx/receiver"
	receiverruntime "github.com/dellarb/mailmoose/dialmx/runtime"
	"github.com/dellarb/mailmoose/internal/mxagent"
)

// buildVersion is stamped at build time with -ldflags. It defaults to "dev".
var buildVersion = "dev"

func main() {
	bootID := newBootID()
	log := receiver.NewLogger(slog.LevelInfo, bootID, os.Stderr)
	slog.SetDefault(log)

	// The included receiver is spawned by the core in standby with an inherited
	// private control channel. When those descriptors are present, run the
	// controlled lifecycle instead of the environment-configured standalone
	// receiver. Standalone, single and shared deployments without the channel
	// are unchanged.
	if control.Enabled() {
		runIncluded(log)
		return
	}
	runStandalone(log)
}

// runIncluded serves the core's standby control channel. The child binds
// nothing until Configure arrives and never exits between activate/deactivate
// cycles; it closes and returns on shutdown, a lost channel, or a process
// signal.
func runIncluded(log *slog.Logger) {
	cmdFile, err := control.CommandFile()
	if err != nil {
		log.Error("included receiver control channel", "error", err)
		os.Exit(2)
	}
	defer cmdFile.Close()
	replyFile, err := control.ReplyFile()
	if err != nil {
		log.Error("included receiver control channel", "error", err)
		os.Exit(2)
	}
	defer replyFile.Close()

	rt := receiverruntime.New(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Closing the command descriptor releases the blocked control read when the
	// process is signalled.
	go func() {
		<-ctx.Done()
		_ = cmdFile.Close()
	}()

	log.Info(receiver.EventReceiverStarting,
		"version", buildVersion,
		"protocol", mxwireProtocol(),
		"pid", os.Getpid(),
		"mode", "included-standby",
	)
	if err := control.Serve(ctx, cmdFile, replyFile, rt, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("included receiver control loop failed", "error", err)
	}
	if err := rt.Close(); err != nil {
		log.Warn("included receiver shutdown", "error", err)
	}
	log.Info(receiver.EventReceiverStopped)
}

// runStandalone runs the environment-configured receiver: one process with the
// SMTP edge and the HTTPS/2 dialer session endpoint. It is used by the
// standalone image and the shared remote deployment.
func runStandalone(log *slog.Logger) {
	started := time.Now()
	log.Info(receiver.EventReceiverStarting,
		"version", buildVersion,
		"protocol", mxwireProtocol(),
		"pid", os.Getpid(),
	)

	cfg, err := dialmx.Load()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(2)
	}
	var cert tls.Certificate
	if cfg.TLSCertFile != "" {
		cert, err = tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile)
		if err != nil {
			log.Error("load TLS certificate", "error", err)
			os.Exit(2)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, stopFD := mxagent.WatchShutdownFD(ctx)
	defer stopFD()

	r := receiver.New(cfg.Receiver, log)

	// The SMTP edge hands each message to the in-process receiver's Delivery.
	// The receiver owns domain registration, so the edge is policy-free.
	edge := mxagent.NewServerWithHandoff(cfg.SMTP, log, func() mxagent.Delivery {
		return r.NewDelivery()
	})

	// The session listener serves the HTTP/2 dialer protocol. A certificate
	// pair selects TLS; without one the listener serves cleartext HTTP/2
	// (prior knowledge), which the receiver admits from loopback and, in shared
	// mode, only from the DIALMX_TRUSTED_PROXIES allowlist. Only the header
	// read is bounded globally; the session itself relies on per-read deadlines
	// and context cancellation so a long-lived dialer is not killed by a
	// blanket server read timeout. ServeTLS is retained: it configures HTTP/2
	// and its ConnState callback already observes the *tls.Conn, so the
	// transport tracker can classify a handshake that fails before any session.
	tracker := receiver.NewTransportTracker(log)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}},
		ConnContext:       tracker.ConnContext,
		ConnState:         tracker.ConnState,
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP2(true)
	if cfg.TLSCertFile == "" {
		srv.Protocols.SetUnencryptedHTTP2(true)
	}

	var wg sync.WaitGroup
	run := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil && ctx.Err() == nil {
				log.Error(receiver.EventListenerFailed, "listener", name, "error", err)
				stop()
			}
		}()
	}

	smtpLn, err := net.Listen("tcp", cfg.SMTP.ListenAddr)
	if err != nil {
		log.Error("listen SMTP", "error", err)
		os.Exit(1)
	}
	logSettings(log, cfg)

	tlsLn, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("listen session", "error", err)
		os.Exit(1)
	}

	log.Info(receiver.EventListenerBound, "listener", "smtp", "addr", smtpLn.Addr().String())
	log.Info(receiver.EventListenerBound, "listener", "session", "addr", tlsLn.Addr().String())
	log.Info(receiver.EventReceiverReady, "boot_duration", time.Since(started).Round(time.Millisecond).String())

	run("mx edge", func() error { return edge.ListenAndServe(ctx, smtpLn) })
	run("session", func() error {
		if cfg.TLSCertFile == "" {
			return srv.Serve(tlsLn)
		}
		return srv.ServeTLS(tlsLn, "", "")
	})

	<-ctx.Done()
	// Either listener failing calls stop via the run wrapper, so shutdown is
	// shared. Bound it so a stuck peer cannot hold the process open.
	log.Info(receiver.EventReceiverStopping)
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
	log.Info(receiver.EventReceiverStopped, "uptime", time.Since(started).Round(time.Millisecond).String())
}

// newBootID returns a random 128-bit hex id that is stable for one process boot.
// It is not a credential and is not accepted as one.
func newBootID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}

// mxwireProtocol is the wire protocol the process speaks. It is referenced from
// the logging envelope rather than from the receiver package so the startup
// record can state the protocol before any session exists.
func mxwireProtocol() string { return "mx-v2" }

// logSettings emits the effective startup settings once: build and protocol,
// listener addresses, message/staging/connection/recipient limits, the
// verification toggles, the DNS resolver and the receiver's own bounds. It
// carries no secrets: certificate paths and key material are never logged.
func logSettings(log *slog.Logger, cfg dialmx.Config) {
	log.Info("dialmx mode", "mode", cfg.Receiver.Mode, "session_tls", cfg.TLSCertFile != "", "trusted_proxies", prefixesString(cfg.Receiver.TrustedProxies))
	log.Info(receiver.EventSettings,
		"version", buildVersion,
		"go", runtime.Version(),
		"protocol", mxwireProtocol(),
		"session_listen", cfg.ListenAddr,
		"smtp_listen", cfg.SMTP.ListenAddr,
		"hostname", cfg.SMTP.Hostname,
		"require_tls", cfg.SMTP.RequireTLS,
		"verify_spf", cfg.SMTP.VerifySPF,
		"verify_dkim", cfg.SMTP.VerifyDKIM,
		"verify_dmarc", cfg.SMTP.VerifyDMARC,
		"dns_resolver", cfg.SMTP.DNSResolver,
		"max_message_bytes", cfg.SMTP.MaxMessageBytes,
		"max_staging_bytes", cfg.SMTP.MaxStagingBytes,
		"max_recipients", cfg.SMTP.MaxRecipients,
		"max_connections", cfg.SMTP.MaxConnections,
		"max_domains_per_connection", cfg.Receiver.MaxDomainsPerConnection,
		"max_transactions", cfg.Receiver.MaxTransactions,
		"max_transactions_per_connection", cfg.Receiver.MaxTransactionsPerConnection,
		"max_transactions_per_domain", cfg.Receiver.MaxTransactionsPerDomain,
		"per_ip_conn_limit", cfg.Receiver.MaxConnsPerIP,
		"per_ip_conn_window_max", cfg.Receiver.ConnWindowMax,
		"per_ip_auth_concurrent", cfg.Receiver.MaxAuthConcurrent,
		"per_ip_auth_window_max", cfg.Receiver.AuthWindowMax,
		"auth_timeout", cfg.Receiver.AuthTimeout.String(),
		"resolve_timeout", cfg.Receiver.ResolveTimeout.String(),
		"ingest_timeout", cfg.Receiver.IngestTimeout.String(),
		"revalidate_interval", cfg.Receiver.RevalidateInterval.String(),
	)
}

// prefixesString renders a prefix allowlist for the settings log. It is public
// configuration, never a secret.
func prefixesString(prefixes []netip.Prefix) string {
	if len(prefixes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		parts = append(parts, p.String())
	}
	return strings.Join(parts, ",")
}
