package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

func TestPrivateMXSessionPersistsAndDeduplicates(t *testing.T) {
	svc, _, _, box := mxService(t)
	r := receiver.New(receiver.Config{Mode: "single", CoreKey: "private-key"}, nil)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	defer srv.Close()
	m := mxdial.New(svc.PrivateMXBackend(), mxdial.Config{DataDir: svc.Config.DataDir, MaxMessageBytes: svc.Config.MaxMessageBytes, ReceiverURL: srv.URL, CoreKey: "private-key"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	var firstID string
	for attempt := 0; attempt < 2; attempt++ {
		delivery := r.NewDelivery()
		deadline := time.Now().Add(5 * time.Second)
		for {
			resolved, err := delivery.Resolve(ctx, []string{box.Address, "unknown@example.com"})
			if err == nil && len(resolved.Results) == 2 && resolved.Results[0].Accept {
				if resolved.Results[1].Accept {
					t.Fatal("unknown recipient accepted")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("private core unavailable: %+v, %v", resolved, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		meta := mxwire.IngestMetadata{Recipients: []string{box.Address}, EnvelopeFrom: "sender@outside.test"}
		result, err := delivery.Ingest(ctx, meta, strings.NewReader(goodRaw), int64(len(goodRaw)), mxwire.BodyDigest([]byte(goodRaw)))
		delivery.Close()
		if err != nil || len(result.PerRecipient) != 1 {
			t.Fatalf("delivery: %+v, %v", result, err)
		}
		rr := result.PerRecipient[0]
		if attempt == 0 {
			if rr.MachineCode != mxwire.CodeOK || rr.Disposition != mxwire.DispositionStored || rr.MessageID == "" {
				t.Fatalf("durable result: %+v", rr)
			}
			firstID = rr.MessageID
		} else if rr.MachineCode != mxwire.CodeDuplicate || rr.MessageID != firstID {
			t.Fatalf("retry was not deduplicated: %+v", rr)
		}
	}
}
