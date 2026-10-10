package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dellarb/mailmoose/internal/admincli"
	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/dnsfallback"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/launcher"
	"github.com/dellarb/mailmoose/internal/logging"
	"github.com/dellarb/mailmoose/internal/privdrop"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func main() {
	// Operator commands run in the same binary but before any server setup, so
	// they do not require a full application configuration (or an encryption
	// key) and never spawn the embedded MX edge.
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(admincli.Run(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	log := logging.New(os.Stdout, slog.LevelInfo, logging.PrefixCore)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		os.Exit(2)
	}
	log.Info("starting", "mode", cfg.Mode, "data_dir", cfg.DataDir)

	// Install ordered resolver failover before any network client or goroutine
	// starts, so outbound provider HTTP, SMTP, direct MX, SPF/DKIM/DMARC and
	// domain checks all recover when the host's configured DNS servers fail. A
	// nil list leaves native DNS untouched.
	net.DefaultResolver = dnsfallback.New(cfg.DNSFallbackServers)
	if len(cfg.DNSFallbackServers) > 0 {
		log.Info("DNS fallback configured", "servers", cfg.DNSFallbackServers)
	}

	runUID, runGID, err := privdrop.ResolvedIdentity()
	if err != nil {
		log.Error("invalid runtime identity", "error", err)
		os.Exit(2)
	}

	// Spawn the included receiver in standby before dropping privileges, so the
	// child runs under a separate uid that can never read /data or the
	// application key. This happens unconditionally when the process is root and
	// an edge binary is present: the child binds nothing until the persisted
	// settings activate it, so an unconfigured (or remote-only) deployment
	// pays only a dormant process. A missing binary or a non-root process is
	// not fatal: the receiver is then remote-only and the admin UI reports the
	// included shape as unavailable.
	edge, edgeErr := startStandbyEdge(cfg, runUID, runGID, log)
	if edgeErr != nil {
		log.Warn("embedded MX edge unavailable; remote receiver remains available", "error", edgeErr)
	}
	if edge != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), launcher.StopTimeout)
			defer cancel()
			if err := edge.Stop(ctx); err != nil {
				log.Warn("embedded mx edge shutdown", "error", err)
			}
		}()
	}

	// Fix up the data directory and shed root before opening the database, so
	// the SQLite files and the message tree are owned by the runtime user. When
	// the process is already non-root this is a no-op.
	dropped, runUID, runGID, err := privdrop.DropToRuntimeUser(cfg.DataDir)
	if err != nil {
		log.Error("privilege drop failed", "error", err)
		os.Exit(1)
	}
	log.Info("runtime identity", "uid", runUID, "gid", runGID, "dropped", dropped)
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
	svc.Log = log
	dialTLS, err := mxdial.ReceiverTLS(cfg.DialMXCAFile)
	if err != nil {
		log.Error("invalid Dial MX TLS configuration", "error", err)
		os.Exit(2)
	}
	// The shared dial manager serves per-domain Dial MX domains. It keeps
	// running regardless of the installation receiver mode: a domain may route
	// to its own receiver. The installation-wide receiver is owned separately by
	// mxRuntime.
	dialManager := mxdial.New(svc.DialMXBackend(), mxdial.Config{DataDir: cfg.DataDir, MaxMessageBytes: cfg.MaxMessageBytes, MaxTransactions: cfg.InboundConcurrency, TLSConfig: dialTLS, AllowPrivateDestinations: !cfg.RequirePublicOutbound()})
	svc.DialMX = dialManager
	svc.InstallDialMXStatusObserver()
	// Antler MX endpoints are resolved live from the repository manifest at
	// setup-save time, cached, and fall back to the embedded copy. The snapshot
	// is stored per domain, so existing setups never move.
	svc.AntlerEndpoints = mxdial.DefaultAntlerResolver()
	dialCtx, dialCancel := context.WithCancel(context.Background())
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		dialManager.Run(dialCtx)
	}()
	defer dialCancel()

	// Transition the legacy environment MX configuration into the store once,
	// then start the process-owned receiver controller. The controller reads the
	// persisted settings and reconciles the included child or the private dialer
	// without ever crashing the core on a receiver failure.
	importLegacyMXSettings(context.Background(), svc, cfg, log)
	mxrt := startMXRuntime(svc, edge, log)
	remoteMXrt := startRemoteMXRuntime(svc, log)
	ensureSystemAdmin(svc, log)
	worker := app.NewOutboxWorker(svc, log)
	worker.Start()
	defer worker.Stop()
	h := httpapp.New(svc, log)

	type listener struct {
		name string
		srv  *http.Server
	}
	// origin is the public URL the listener is reached through, so the startup
	// log names the URL that actually serves this listener rather than a single
	// shared base_url that may not match a reverse proxy or receiver hostname.
	// note is a short human-facing clarifier appended to the startup line when a
	// listener's purpose is not obvious from its label alone.
	start := func(name, note, addr, origin string, handler http.Handler, tlsCert, tlsKey string) (*listener, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("%s listener: %w", name, err)
		}
		if tlsCert != "" && tlsKey != "" {
			cert, cerr := tls.LoadX509KeyPair(tlsCert, tlsKey)
			if cerr != nil {
				ln.Close()
				return nil, fmt.Errorf("%s listener TLS: %w", name, cerr)
			}
			ln = tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.BodyReadTimeout, IdleTimeout: 90 * time.Second}
		go func() {
			attrs := []any{"listener", name, "addr", ln.Addr().String(), "origin", origin}
			// The main listener never terminates TLS, so only the receiver
			// reports it (and only when actually configured).
			if tlsCert != "" && tlsKey != "" {
				attrs = append(attrs, "tls", true)
			}
			if note != "" {
				attrs = append(attrs, logging.Literal("note", note))
			}
			log.Info("listening", attrs...)
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Error("HTTP server failed", "listener", name, "error", err)
				os.Exit(1)
			}
		}()
		return &listener{name: name, srv: srv}, nil
	}

	mainListener, err := start("ui+api", "", cfg.ListenAddr, cfg.BaseURL, h.Handler(), "", "")
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	// The inbound connector can optionally serve TLS so a remote MX edge reaches
	// it over verified TLS.
	listeners := []*listener{mainListener}
	if cfg.DedicatedReceiverEnable {
		inboundListener, err := start("receiver-only", "(Incoming mail webhooks only)", fmt.Sprintf(":%d", cfg.DedicatedReceiverPort), cfg.ReceiverURL(), h.InboundHandler(), cfg.InboundTLSCertFile, cfg.InboundTLSKeyFile)
		if err != nil {
			log.Error("startup failed", "error", err)
			os.Exit(1)
		}
		listeners = append(listeners, inboundListener)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	// An unexpected embedded edge exit is reported but not fatal: the core's
	// mailboxes, API and webhook ingest stay up, and the MX receiver status
	// reports the failure so the operator can act. A remote receiver is
	// unaffected. This is the "no core crash on unused child failure" contract.
	go func() {
		if ch := edgeExit(edge); ch != nil {
			<-ch
			if err := edge.ExitError(); err != nil {
				log.Error("embedded mx edge exited unexpectedly", "error", err)
			}
		}
	}()
	<-stop
	dialCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, l := range listeners {
		if err := l.srv.Shutdown(ctx); err != nil {
			log.Warn("HTTP shutdown failed", "listener", l.name, "error", err)
		}
	}
	select {
	case <-dialDone:
	case <-ctx.Done():
		log.Warn("Dial MX shutdown timed out")
	}
	// Stop the MX receiver and wait (bounded) for the included child to drain
	// and the private dialer to stop, before the deferred store close runs. The
	// bound is the child's stop grace plus a small margin.
	mxCtx, mxCancel := context.WithTimeout(context.Background(), launcher.StopTimeout+5*time.Second)
	defer mxCancel()
	if err := mxrt.Shutdown(mxCtx); err != nil {
		log.Warn("MX receiver shutdown timed out", "error", err)
	}
	// Stop the per-account Remote MX dialers before the store closes.
	if err := remoteMXrt.Shutdown(mxCtx); err != nil {
		log.Warn("Remote MX receiver shutdown timed out", "error", err)
	}
}

// ensureSystemAdmin reconciles the configured system administrator
// (ADMIN_EMAIL / ADMIN_PASSWORD, either of which may come from a *_FILE secret)
// with the database. The configured credentials are authoritative when present:
// they create the system administrator on first start and rotate its stored
// login (revoking its sessions) when either changes. When they are absent the
// stored system administrator is left untouched, so a deployment may drop them
// once provisioned. A half-configured pair is a startup error and never reaches
// this function. The password is never logged.
func ensureSystemAdmin(svc *app.Service, log *slog.Logger) {
	if svc.Config.AdminEmail != "" {
		u, changed, err := svc.Store.SyncSystemAdmin(context.Background(), svc.Config.AdminAccountName, svc.Config.AdminEmail, svc.Config.AdminPassword, svc.Config.DefaultQuotaBytes)
		if err != nil {
			log.Error("cannot configure system administrator", "error", err)
			os.Exit(1)
		}
		if changed {
			log.Info("system administrator configured", "email", u.Email, "updated", true)
		}
		return
	}
	has, err := svc.Store.HasSystemAdmin(context.Background())
	if err != nil {
		log.Error("cannot determine system administrator state", "error", err)
		os.Exit(1)
	}
	if !has {
		log.Info("MailMoose has not been configured; complete the first-run setup page, or set ADMIN_EMAIL and ADMIN_PASSWORD and restart")
	}
}

// edgeExit returns the edge's exit channel, or a nil channel (blocks forever)
// when no embedded edge is running, so the select only wakes for signals.
func edgeExit(edge *launcher.Edge) <-chan struct{} {
	if edge == nil {
		return nil
	}
	return edge.Wait()
}

// startStandbyEdge spawns the included receiver as a standby child under a
// separate uid, before the core drops privileges. The child binds no listeners
// and holds no credential until the persisted settings activate it over the
// private control channel, so no MX_ENABLE value or bootstrap secret is needed
// to start it: an unconfigured deployment simply leaves it dormant. It returns
// an error (and a nil edge) when the process is not root or the child cannot be
// isolated; that is not fatal because remote mode remains available.
func startStandbyEdge(cfg config.Config, runUID, runGID int, log *slog.Logger) (*launcher.Edge, error) {
	if os.Getuid() != 0 {
		return nil, fmt.Errorf("the container must start as root so the embedded edge can run under a separate uid; " +
			"remove a strict `user:`/`cap_drop: [ALL]` from the service, or use remote mode with the mailmoose-mx container (see README.md)")
	}
	if cfg.MXUID == runUID || cfg.MXGID == runGID {
		return nil, fmt.Errorf("MX_UID/MX_GID must differ from the app runtime uid/gid (%d:%d) for the edge isolation to be meaningful", runUID, runGID)
	}
	hostname := edgeHostname()
	spec := launcher.Spec{
		Binary:   launcher.ResolveBinary(),
		UID:      cfg.MXUID,
		GID:      cfg.MXGID,
		Hostname: hostname,
		Env:      launcher.StandbyEnv(3, 4),
	}
	return launcher.StartStandby(context.Background(), spec, log)
}

func edgeHostname() string {
	if v := os.Getenv("MX_HOSTNAME"); v != "" {
		return v
	}
	return "mailmoose-mx"
}
