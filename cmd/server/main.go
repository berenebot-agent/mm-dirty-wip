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
	"strings"
	"syscall"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/launcher"
	"gatehouse-mail/internal/logging"
	"gatehouse-mail/internal/privdrop"
	"gatehouse-mail/internal/store"
)

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo, logging.PrefixCore)

	// In the embedded single-container mode, generate (or select) the edge
	// credential BEFORE config.Load so the core's MX_EDGE_KEYS includes it and
	// authorizes the edge this process is about to spawn.
	installEmbeddedCredential(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("configuration error", "error", err)
		os.Exit(2)
	}
	if cfg.TrustProxyHeaders && len(cfg.TrustedProxies) == 0 {
		log.Warn("TRUST_PROXY_HEADERS=true trusts X-Forwarded-* headers from any peer; prefer TRUSTED_PROXIES with your reverse proxy's address")
	}

	runUID, runGID, err := privdrop.ResolvedIdentity()
	if err != nil {
		log.Error("invalid runtime identity", "error", err)
		os.Exit(2)
	}

	// Embedded MX: in embedded mode, spawn the edge as a separate process under a
	// different uid before dropping privileges, then supervise it. Remote mode
	// leaves MXEmbedded false and the edge runs separately.
	var edge *launcher.Edge
	if cfg.MXReceiveEnabled && cfg.MXEmbedded {
		edge, err = startEmbeddedEdge(cfg, runUID, runGID, log)
		if err != nil {
			log.Error("cannot start embedded MX edge", "error", err)
			os.Exit(1)
		}
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
	worker := app.NewOutboxWorker(svc, log)
	worker.Start()
	defer worker.Stop()
	h := httpapp.New(svc, log)

	type listener struct {
		name string
		srv  *http.Server
	}
	start := func(name, addr string, handler http.Handler, tlsCert, tlsKey string) (*listener, error) {
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
			log.Info("Gatehouse Mail listening", "listener", name, "addr", ln.Addr().String(), "mode", cfg.Mode, "base_url", cfg.BaseURL)
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Error("HTTP server failed", "listener", name, "error", err)
				os.Exit(1)
			}
		}()
		return &listener{name: name, srv: srv}, nil
	}

	mainListener, err := start("main", cfg.ListenAddr, h.Handler(), "", "")
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	// The inbound connector can optionally serve TLS so a remote MX edge reaches
	// it over verified TLS.
	inboundListener, err := start("inbound", config.InboundAddr, h.InboundHandler(), cfg.InboundTLSCertFile, cfg.InboundTLSKeyFile)
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	listeners := []*listener{mainListener, inboundListener}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
	case err := <-edgeExit(edge):
		// An unexpected edge exit is fatal: the core would keep running and
		// silently stop receiving direct SMTP. Stop so the container restarts
		// or the operator notices.
		log.Error("embedded mx edge exited unexpectedly", "error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, l := range listeners {
		if err := l.srv.Shutdown(ctx); err != nil {
			log.Warn("HTTP shutdown failed", "listener", l.name, "error", err)
		}
	}
}

// edgeExit returns the edge's exit channel, or a nil channel (blocks forever)
// when no embedded edge is running, so the select only wakes for signals.
func edgeExit(edge *launcher.Edge) <-chan error {
	if edge == nil {
		return nil
	}
	return edge.Wait()
}

// installEmbeddedCredential selects or generates the edge credential and, when
// the operator supplied none, exports MX_EDGE_KEYS so config.Load accepts it.
// The derived credential is also stashed in an env var the spawner reads, so
// both the core and the child agree without the operator setting anything.
// Only embedded mode (MX_ENABLE=true) embeds an edge.
func installEmbeddedCredential(log *slog.Logger) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MX_ENABLE"))) {
	case "true", "local", "on", "1", "yes":
	default:
		return
	}
	keyID, secret := launcher.ResolveEdgeCredential(parseEnvEdgeKeys(os.Getenv("MX_EDGE_KEYS")))
	if v := os.Getenv("MX_EDGE_KEYS"); v == "" {
		_ = os.Setenv("MX_EDGE_KEYS", keyID+":"+secret)
		log.Info("embedded mx edge credential generated", "key_id", keyID)
	} else {
		log.Info("embedded mx edge credential selected", "key_id", keyID)
	}
}

// startEmbeddedEdge validates the privilege requirements and spawns the edge.
// It refuses when the process cannot separate the edge's uid from the app's
// runtime uid, with the two concrete remedies.
func startEmbeddedEdge(cfg config.Config, runUID, runGID int, log *slog.Logger) (*launcher.Edge, error) {
	if os.Getuid() != 0 {
		return nil, fmt.Errorf("MX_ENABLE=true requires the container to start as root so the edge can run under a separate uid; " +
			"remove a strict `user:`/`cap_drop: [ALL]` from the service, or set MX_ENABLE=remote and run the gatehouse-mx container (docker-compose.mx-sidecar.yml) for hard isolation")
	}
	if cfg.MXUID == runUID || cfg.MXGID == runGID {
		return nil, fmt.Errorf("MX_UID/MX_GID must differ from the app runtime uid/gid (%d:%d) for the edge isolation to be meaningful", runUID, runGID)
	}
	keyID, secret := launcher.ResolveEdgeCredential(cfg.MXEdgeKeys)
	hostname := edgeHostname()
	spec := launcher.Spec{
		Binary:   launcher.ResolveBinary(),
		UID:      cfg.MXUID,
		GID:      cfg.MXGID,
		KeyID:    keyID,
		Secret:   secret,
		Hostname: hostname,
		Env:      launcher.EdgeEnv(hostname, keyID, secret, 3),
	}
	return launcher.Start(context.Background(), spec, log)
}

// parseEnvEdgeKeys parses the same key_id:secret list config.Load uses, kept
// local so cmd/server does not depend on config internals.
func parseEnvEdgeKeys(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, secret, ok := strings.Cut(part, ":")
		id, secret = strings.TrimSpace(id), strings.TrimSpace(secret)
		if ok && id != "" && secret != "" {
			out[id] = secret
		}
	}
	return out
}

func edgeHostname() string {
	if v := os.Getenv("MX_HOSTNAME"); v != "" {
		return v
	}
	return "gatehouse-mx"
}
