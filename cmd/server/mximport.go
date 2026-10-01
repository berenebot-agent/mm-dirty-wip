package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/model"
)

// importLegacyMXSettings performs the one-time transition from the legacy MX
// environment (MX_ENABLE, MX_RECEIVER_URL, DIALMX_CORE_KEY and the MX_VERIFY_*
// toggles) to the persisted mx_settings row. It runs once, after the database is
// open:
//
//   - if the store already holds a configuration, the environment is ignored
//     entirely, so a stale or partial value left behind in a deployment cannot
//     override the UI or block startup;
//   - otherwise, when MX_ENABLE named a receiver, it is imported and validated
//     through the same SaveMXReceiverSettings path the UI uses (generating the
//     included child's bearer key and preserving the operator's verification
//     choices);
//   - a partial remote environment is rejected here, once, rather than at every
//     Load, because after this row exists the environment is irrelevant;
//   - otherwise nothing is written, leaving the store uninitialized so a later
//     deliberate environment change can still be imported.
//
// Import is best-effort with respect to the running receiver: the runtime
// reconciles whatever ends up persisted.
func importLegacyMXSettings(ctx context.Context, svc *app.Service, cfg config.Config, log *slog.Logger) {
	initialized, err := svc.Store.MXSettingsInitialized(ctx)
	if err != nil {
		log.Error("cannot determine MX settings state", "error", err)
		return
	}
	envSet := cfg.MXImport.Set || strings.TrimSpace(cfg.MXReceiverURL) != "" || strings.TrimSpace(cfg.MXCoreKey) != ""
	if initialized {
		if envSet {
			log.Info("MX environment ignored: receiver configuration is managed in the admin UI")
		}
		return
	}
	if !cfg.MXImport.Set {
		// No MX_ENABLE, but a stray URL/key is ambiguous: importing half a
		// remote configuration would fail validation, so it is reported and
		// skipped rather than guessed at.
		if strings.TrimSpace(cfg.MXReceiverURL) != "" || strings.TrimSpace(cfg.MXCoreKey) != "" {
			log.Warn("MX_RECEIVER_URL/DIALMX_CORE_KEY set without MX_ENABLE; set MX_ENABLE=remote or configure the receiver in the admin UI")
		}
		return
	}
	input := app.MXReceiverInput{
		Mode:      cfg.MXImport.Mode,
		URL:       cfg.MXImport.ReceiverURL,
		BearerKey: cfg.MXImport.CoreKey,
	}
	if cfg.MXImport.Mode == config.MXModeIncludedSetting {
		input.Hostname = cfg.MXImport.Hostname
		input.MaxMessageBytes = cfg.MXImport.MaxMessageBytes
		input.MaxStagingBytes = cfg.MXImport.MaxStagingBytes
		input.MaxRecipients = cfg.MXImport.MaxRecipients
		input.MaxConnections = cfg.MXImport.MaxConnections
		input.RequireTLS = cfg.MXImport.RequireTLS
		input.VerifySPF = cfg.MXImport.VerifySPF
		input.VerifyDKIM = cfg.MXImport.VerifyDKIM
		input.VerifyDMARC = cfg.MXImport.VerifyDMARC
		input.DNSResolver = cfg.MXImport.DNSResolver
		input.DNSTimeoutSeconds = cfg.MXImport.DNSTimeoutSeconds
		input.ReadTimeoutSeconds = cfg.MXImport.ReadTimeoutSeconds
		input.WriteTimeoutSeconds = cfg.MXImport.WriteTimeoutSeconds
		input.DataTimeoutSeconds = cfg.MXImport.DataTimeoutSeconds
		// The legacy STARTTLS files are read once, here, so the persisted
		// configuration carries the PEM and the child never needs the paths. A
		// partial or unreadable pair is NOT silently dropped: the whole import is
		// aborted so the receiver is left unconfigured for the operator to fix,
		// rather than persisting a receiver that silently lost its STARTTLS.
		cert, key, attempted, terr := readLegacyTLSPair(cfg.MXImport.TLSCertFile, cfg.MXImport.TLSKeyFile)
		if terr != nil {
			log.Error("cannot import legacy MX configuration: STARTTLS files unusable", "error", terr)
			return
		}
		if attempted {
			input.SMTPTLSCert = cert
			input.SMTPTLSKey = key
		}
	}
	if _, err := svc.SaveMXReceiverSettings(ctx, model.Principal{SystemAdmin: true}, input); err != nil {
		// The error is safe to log: validation messages never echo the private
		// key, and SaveMXReceiverSettings never returns decrypted material.
		log.Error("cannot import legacy MX configuration", "error", err)
		return
	}
	log.Info("imported legacy MX configuration", "mode", cfg.MXImport.Mode)
}

// readLegacyTLSPair reads the legacy MX_TLS_CERT/MX_TLS_KEY files for the
// one-time import. attempted reports whether TLS was requested (either path
// set). It returns an error when only one path is set or a file cannot be read;
// the caller then aborts the whole import, so a receiver is never persisted with
// its STARTTLS silently dropped.
func readLegacyTLSPair(certFile, keyFile string) (cert, key string, attempted bool, err error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)
	if certFile == "" && keyFile == "" {
		return "", "", false, nil
	}
	if (certFile == "") != (keyFile == "") {
		return "", "", true, errors.New("MX_TLS_CERT and MX_TLS_KEY must be set together")
	}
	certBytes, rerr := os.ReadFile(certFile)
	if rerr != nil {
		return "", "", true, fmt.Errorf("read MX_TLS_CERT: %w", rerr)
	}
	keyBytes, rerr := os.ReadFile(keyFile)
	if rerr != nil {
		return "", "", true, fmt.Errorf("read MX_TLS_KEY: %w", rerr)
	}
	return strings.TrimSpace(string(certBytes)), strings.TrimSpace(string(keyBytes)), true, nil
}
