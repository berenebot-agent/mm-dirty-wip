package one_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/one"
)

func TestMetadataSyncWaitStatusIsRequestScopedAndCleared(t *testing.T) {
	l := &one.Limiter{Interval: time.Hour}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	states := make(chan bool, 2)
	ctx = one.WithWaitStatus(ctx, func(waiting bool) { states <- waiting })
	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx) }()
	select {
	case waiting := <-states:
		if !waiting {
			t.Fatal("wait not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("no provider wait status")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request submitted")
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not cancel")
	}
	if waiting := <-states; waiting {
		t.Fatal("cancelled wait leaves spinner stuck")
	}
}
