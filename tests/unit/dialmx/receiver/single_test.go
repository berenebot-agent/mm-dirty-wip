package receiver_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func TestSingleCoreSessionDelivery(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("TLS=%v", encrypted), func(t *testing.T) {
			r := newReceiver(t, receiver.Config{Mode: "single", CoreKey: "private-key", LookupTXT: func(context.Context, string) ([]string, error) {
				t.Error("single mode performed domain DNS authentication")
				return nil, nil
			}})
			srv := httptest.NewUnstartedServer(r.Handler())
			srv.Config.Protocols = new(http.Protocols)
			srv.Config.Protocols.SetHTTP2(true)
			srv.Config.Protocols.SetUnencryptedHTTP2(true)
			cfg := mxdial.Config{DataDir: t.TempDir(), CoreKey: "private-key", ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true}
			if encrypted {
				srv.EnableHTTP2 = true
				srv.StartTLS()
				cfg.TLSConfig = rootTLS(t, srv)
			} else {
				srv.Start()
			}
			defer srv.Close()
			cfg.ReceiverURL = srv.URL
			be := &backend{reject: map[string]bool{"unknown@other.test": true}}
			manager := mxdial.New(be, cfg)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); manager.Run(ctx) }()
			defer func() { cancel(); <-done }()
			delivery := r.NewDelivery()
			defer delivery.Close()
			var resolved mxwire.ResolveResponse
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				var err error
				resolved, err = delivery.Resolve(ctx, []string{"a@example.test", "b@other.test", "unknown@other.test"})
				if err == nil && len(resolved.Results) == 3 && resolved.Results[0].Accept {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if len(resolved.Results) != 3 || !resolved.Results[0].Accept || !resolved.Results[1].Accept || resolved.Results[2].Accept {
				t.Fatalf("resolution: %+v", resolved)
			}
			raw := "From: sender@outside.test\r\nSubject: private\r\n\r\nbody"
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
			result, err := delivery.Ingest(ctx, mxwire.IngestMetadata{Recipients: []string{"a@example.test", "b@other.test"}}, strings.NewReader(raw), int64(len(raw)), digest)
			if err != nil || len(result.PerRecipient) != 2 {
				t.Fatalf("ingest: %+v, %v", result, err)
			}
			be.mu.Lock()
			if be.raw != raw || len(be.stored) != 2 {
				t.Errorf("stored=%v, raw=%q", be.stored, be.raw)
			}
			be.mu.Unlock()
			registerReceiver(srv.URL, r)
			send(t, startEdge(t, 16, srv.URL, cfg.TLSConfig), []string{"smtp@new-domain.test"})
			cancel()
			<-done
			unavailable := r.NewDelivery()
			defer unavailable.Close()
			res, err := unavailable.Resolve(context.Background(), []string{"a@example.test"})
			if err == nil && len(res.Results) == 1 && res.Results[0].Accept {
				t.Fatal("disconnected core accepted a recipient")
			}
		})
	}
}

func TestSingleCoreRejectsInvalidBearer(t *testing.T) {
	r := newReceiver(t, receiver.Config{Mode: "single", CoreKey: "private-key"})
	for _, header := range []string{"", "Bearer wrong-key", "Basic private-key"} {
		q := httptest.NewRequest(http.MethodPost, mxwire.SessionPath, nil)
		q.ProtoMajor = 2
		q.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, q)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("header %q: status %d", header, w.Code)
		}
	}
}
