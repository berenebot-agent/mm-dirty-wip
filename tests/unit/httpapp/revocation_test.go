package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEventsStreamCancelledOnKeyRevoke(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "stream", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.Store.APIKeyPrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(h)
	defer ts.Close()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}

	// Revoking the key and cancelling its scope must terminate the open stream.
	if err := svc.Store.RevokeAPIKey(ctx, u.AccountID, p.APIKeyID); err != nil {
		t.Fatal(err)
	}
	svc.Hub.CancelScope("key:" + p.APIKeyID)

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("events stream was not cancelled after key revocation")
	}
}
