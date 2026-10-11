package one_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/one"
	"github.com/dellarb/mailmoose/tests/support/onefixture"
)

func TestDiscoveryAndSharedNativeGmailTransport(t *testing.T) {
	f := onefixture.New(t)
	c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
	ctx := context.Background()
	rows, err := c.Connections(ctx)
	if err != nil || len(rows) != 3 || !rows[0].Usable() || rows[1].Usable() || rows[2].Usable() {
		t.Fatalf("discovery: %+v %v", rows, err)
	}
	encoded, _ := json.Marshal(rows)
	if bytes.Contains(encoded, []byte(onefixture.Selector)) {
		t.Fatal("vault selector serialized")
	}
	if err := c.ValidateActions(ctx); err != nil {
		t.Fatal(err)
	}
	g := c.Gmail(rows[0].Key, nil, nil)
	profile, err := g.Profile(ctx, "")
	if err != nil || profile.EmailAddress != onefixture.Email {
		t.Fatal("native profile unavailable", err)
	}
	var raw bytes.Buffer
	if err := g.Raw(ctx, "", "message-one", &raw); err != nil || !strings.Contains(raw.String(), "Synthetic fixture body") {
		t.Fatal("native raw MIME unavailable", err)
	}
	draft, err := g.CreateDraft(ctx, "", strings.NewReader("From: fixture@example.test\r\nSubject: Draft\r\n\r\nSynthetic"), "")
	if err != nil || draft.ID != "draft-one" {
		t.Fatal("streamed draft unavailable", err)
	}
	sent, err := g.SendRaw(ctx, "", strings.NewReader("From: fixture@example.test\r\nSubject: Send\r\n\r\nSynthetic"), "")
	if err != nil || sent.ID != "sent-one" {
		t.Fatal("streamed send unavailable", err)
	}
	for _, path := range []string{"/gmail/v1/users/me/messages/../profile", "/gmail/v1/users/me/messages/%2Fprofile", "/gmail/v1/users/other/messages", "/gmail/v1/users/me/messages/message-one"} {
		method := "GET"
		if strings.HasSuffix(path, "message-one") {
			method = "DELETE"
		}
		req, _ := http.NewRequest(method, "https://gmail.googleapis.com"+path, nil)
		if _, err := g.Executor.Execute(ctx, "injected", req); err == nil {
			t.Fatal("allowed injected/permanently destructive operation", path)
		}
	}
}

func TestActionMappingFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "wrong-method", "wrong-path"} {
		t.Run(mode, func(t *testing.T) {
			f := onefixture.New(t)
			f.Change(func(f *onefixture.Fixture) {
				switch mode {
				case "missing":
					f.Actions = f.Actions[1:]
				case "duplicate":
					f.Actions = append(f.Actions, f.Actions[0])
				case "wrong-method":
					f.Actions[0].Method = "POST"
				case "wrong-path":
					f.Actions[0].Path = "/v1/gmail/profile"
				}
			})
			c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
			if _, err := c.Gmail(onefixture.Selector, nil, nil).Profile(context.Background(), ""); err == nil {
				t.Fatal("unverified action executed")
			}
		})
	}
}

func TestOneAndGmailErrorsAreDistinctAndWritesNotRetried(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, code := range []int{401, 403, 404, 429, 503} {
			f := onefixture.New(t)
			f.Change(func(f *onefixture.Fixture) {
				f.NativeErrors = native
				f.Status["GET /v1/passthrough/gmail/v1/users/me/history"] = code
				f.Status["POST /v1/passthrough/gmail/v1/users/me/messages/send"] = code
			})
			c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
			if err := c.ValidateActions(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			g := c.Gmail(onefixture.Selector, nil, nil)
			_, err := g.History(ctx, "", "10", "")
			cancel()
			var mb *model.MailboxError
			if !errors.As(err, &mb) || (mb.Kind == model.ErrKindNotFound) != (native && code == 404) {
				t.Fatalf("source classification code=%d native=%v: %v", code, native, err)
			}
			if strings.Contains(err.Error(), onefixture.Key) || strings.Contains(err.Error(), "sensitive fixture") {
				t.Fatal("unsafe remote error")
			}
			if code == 429 {
				continue
			}
			// Independent fixture client: the preceding GET 5xx intentionally
			// put its shared limiter into cooldown. Exercise submission itself.
			writeClient := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
			_, err = writeClient.Gmail(onefixture.Selector, nil, nil).SendRaw(context.Background(), "", strings.NewReader("Subject: fixture\r\n\r\nbody"), "")
			if code == 503 && !transport.AsAmbiguous(err) {
				t.Fatal("5xx write could be blindly retried", err)
			}
		}
	}
}

func TestDisconnectCheckRunsAgainAfterQueueWait(t *testing.T) {
	f := onefixture.New(t)
	c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
	if err := c.ValidateActions(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks := 0
	g := c.Gmail(onefixture.Selector, func(context.Context) error {
		checks++
		if checks > 1 {
			return model.NewMailboxError(model.ErrKindAuth, "disconnected", false, nil)
		}
		return nil
	}, nil)
	_, err := g.SendRaw(context.Background(), "", strings.NewReader("Subject: fixture\r\n\r\nbody"), "")
	if err == nil || transport.AsAmbiguous(err) || checks != 2 {
		t.Fatal("disconnected queued send submitted", err)
	}
	f.Change(func(f *onefixture.Fixture) {
		if f.SendCalls != 0 {
			t.Error("send reached upstream")
		}
	})
}

func TestLimiterCancellationAndUserPriority(t *testing.T) {
	l := &one.Limiter{Interval: 50 * time.Millisecond}
	if err := l.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx); err == nil {
		t.Fatal("canceled request granted")
	}
	order := make(chan string, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := l.Wait(one.Background(context.Background())); err == nil {
			order <- "sync"
		}
	}()
	go func() {
		defer wg.Done()
		if err := l.Wait(context.Background()); err == nil {
			order <- "user"
		}
	}()
	wg.Wait()
	if first := <-order; first != "user" {
		t.Fatal("sync displaced user operation")
	}
}

type trip func(*http.Request) (*http.Response, error)

func (f trip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRedirectNeverForwardsCredentials(t *testing.T) {
	calls := 0
	h := &http.Client{Transport: trip(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://attacker.example/"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})}
	c := one.NewClient(onefixture.Key, h, nil)
	if _, err := c.Connections(context.Background()); err == nil || calls != 1 {
		t.Fatal("followed credential redirect")
	}
}
