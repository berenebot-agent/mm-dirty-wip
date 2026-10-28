package receiver

// Internal access lets synctest exercise real authentication phases with virtual
// time and no sockets; public HTTP fixture tests cannot advance a fake clock.
import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dellarb/mammothmail/internal/mxagent"
	"github.com/dellarb/mammothmail/internal/mxwire"
)

func TestAuthenticationDNSPhases(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"initial TXT", "verification TXT", "MX", "renewal TXT"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				pub, priv, _ := ed25519.GenerateKey(rand.Reader)
				lookups := 0
				r := New(Config{
					Mode: "shared", AuthTimeout: 15 * time.Second,
					SMTP: mxagent.Config{Hostname: "mx.test"},
					LookupTXT: func(ctx context.Context, _ string) ([]string, error) {
						lookups++
						if (phase == "initial TXT" && lookups == 1) || (phase == "verification TXT" && lookups == 2) || (phase == "renewal TXT" && lookups == 3) {
							select {
							case <-time.After(11 * time.Second):
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						}
						return []string{mxwire.DomainTXT("key1", pub)}, nil
					},
					LookupMX: func(ctx context.Context, _ string) ([]*net.MX, error) {
						if phase == "MX" {
							select {
							case <-time.After(11 * time.Second):
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						}
						return []*net.MX{{Host: "mx.test."}}, nil
					},
				}, nil)
				c := timingConnection(t, r)
				f, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: "example.test", KeyID: "key1"})
				if err := r.auth(c, f); err != nil {
					t.Fatal(err)
				}
				if phase == "initial TXT" {
					time.Sleep(12 * time.Second)
				}
				synctest.Wait()
				r.expireConn(c)
				answerTimingChallenge(t, r, c, priv)
				time.Sleep(12 * time.Second)
				synctest.Wait()
				assertTimingGrant(t, r, c)
				if phase == "renewal TXT" {
					time.Sleep(4 * time.Minute)
					r.revalidateConn(c)
					answerTimingChallenge(t, r, c, priv)
					time.Sleep(time.Second)
					synctest.Wait()
					r.revalidateConn(c)
					r.expireConn(c)
					if unreadTimingBytes(c) != 0 {
						t.Fatal("issued overlapping renewal or expired in-flight verification")
					}
					time.Sleep(11 * time.Second)
					synctest.Wait()
					assertTimingGrant(t, r, c)
				}
			})
		})
	}
}

func timingConnection(t *testing.T, r *Receiver) *connection {
	t.Helper()
	body := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.ips["127.0.0.1"] = &ipState{}
	return &connection{
		id: "connection", receiver: "receiver", ip: "127.0.0.1", ctx: ctx, cancel: cancel,
		writer:  &lockedWriter{w: body, ctl: http.NewResponseController(body)},
		domains: map[uint64]*binding{}, challenges: map[uint64]challenge{}, pending: map[uint64]*pending{},
	}
}

// timingFrame consumes the next frame from the connection's response recorder,
// and unreadTimingBytes reports what is left unread, both under the writer's
// lock: verification runs on a spawned goroutine that writes through
// lockedWriter, and httptest.ResponseRecorder is not safe for concurrent use.
func timingFrame(t *testing.T, c *connection) (mxwire.Frame, error) {
	t.Helper()
	w := c.writer
	w.mu.Lock()
	defer w.mu.Unlock()
	return mxwire.ReadFrame(w.w.(*httptest.ResponseRecorder).Body)
}

func unreadTimingBytes(c *connection) int {
	w := c.writer
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.(*httptest.ResponseRecorder).Body.Len()
}

func answerTimingChallenge(t *testing.T, r *Receiver, c *connection, priv ed25519.PrivateKey) {
	t.Helper()
	f, err := timingFrame(t, c)
	if err != nil || f.Type != mxwire.FrameChallenge {
		t.Fatalf("challenge: %v, frame %v", err, f.Type)
	}
	var challenge mxwire.Challenge
	if err := mxwire.DecodeFrame(f, &challenge); err != nil {
		t.Fatal(err)
	}
	sig, err := mxwire.SignChallenge(priv, challenge)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := mxwire.JSONFrame(mxwire.FrameChallengeResponse, 0, f.ChannelID, mxwire.ChallengeResponse{Domain: challenge.Domain, KeyID: challenge.KeyID, Nonce: challenge.Nonce, Signature: sig})
	if err := r.proof(c, response); err != nil {
		t.Fatal(err)
	}
	// Let verification start without consuming its virtual-time delay.
	synctest.Wait()
}

func assertTimingGrant(t *testing.T, r *Receiver, c *connection) {
	t.Helper()
	f, err := timingFrame(t, c)
	if err != nil || f.Type != mxwire.FrameAuthResult {
		t.Fatalf("result: %v, frame %v", err, f.Type)
	}
	var result mxwire.AuthResult
	if err := mxwire.DecodeFrame(f, &result); err != nil || !result.Accepted {
		t.Fatalf("grant: %+v, error %v", result, err)
	}
	if _, err := r.lookup("example.test"); err != nil {
		t.Fatal(err)
	}
}

func TestRetiredAuthenticationCannotGrant(t *testing.T) {
	t.Parallel()
	for _, retire := range []string{"unregister", "new attempt"} {
		t.Run(retire, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				pub, priv, _ := ed25519.GenerateKey(rand.Reader)
				release := make(chan struct{})
				r := New(Config{Mode: "shared", SMTP: mxagent.Config{Hostname: "mx.test"},
					LookupTXT: func(context.Context, string) ([]string, error) {
						return []string{mxwire.DomainTXT("key1", pub)}, nil
					},
					LookupMX: func(ctx context.Context, _ string) ([]*net.MX, error) {
						select {
						case <-release:
							return []*net.MX{{Host: "mx.test."}}, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					},
				}, nil)
				c := timingConnection(t, r)
				f, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: "example.test", KeyID: "key1"})
				if err := r.auth(c, f); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				answerTimingChallenge(t, r, c, priv)
				if retire == "unregister" {
					f, _ = mxwire.JSONFrame(mxwire.FrameDomainUnregister, 0, 1, mxwire.DomainNotice{Domain: "example.test"})
					if err := r.unregister(c, f); err != nil {
						t.Fatal(err)
					}
				} else {
					f.ChannelID = 2
					if err := r.auth(c, f); err != nil {
						t.Fatal(err)
					}
				}
				close(release)
				synctest.Wait()
				if _, err := r.lookup("example.test"); err == nil {
					t.Fatal("retired authentication installed a binding")
				}
			})
		})
	}
}

func TestAuthenticationRecoversAfterDNSFailure(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		failed := false
		r := New(Config{Mode: "shared", SMTP: mxagent.Config{Hostname: "mx.test"},
			LookupTXT: func(context.Context, string) ([]string, error) {
				if !failed {
					failed = true
					return nil, errors.New("temporary DNS failure")
				}
				return []string{mxwire.DomainTXT("key1", pub)}, nil
			},
			LookupMX: func(context.Context, string) ([]*net.MX, error) {
				return []*net.MX{{Host: "mx.test."}}, nil
			},
		}, nil)
		c := timingConnection(t, r)
		f, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: "example.test", KeyID: "key1"})
		if err := r.auth(c, f); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		result, err := timingFrame(t, c)
		var rejected mxwire.AuthResult
		if err != nil || mxwire.DecodeFrame(result, &rejected) != nil || rejected.Accepted || rejected.Reason != "dns_unavailable" {
			t.Fatalf("expected DNS rejection: %+v, %v", rejected, err)
		}
		time.Sleep(ipFailureCooldown + time.Second)
		f.ChannelID = 2
		if err := r.auth(c, f); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		answerTimingChallenge(t, r, c, priv)
		synctest.Wait()
		assertTimingGrant(t, r, c)
	})
}

func TestAuthenticationFreshnessAndVerificationRemainBounded(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"late response", "verification timeout"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				pub, priv, _ := ed25519.GenerateKey(rand.Reader)
				r := New(Config{Mode: "shared", AuthTimeout: time.Minute, SMTP: mxagent.Config{Hostname: "mx.test"},
					LookupTXT: func(context.Context, string) ([]string, error) {
						return []string{mxwire.DomainTXT("key1", pub)}, nil
					},
					LookupMX: func(ctx context.Context, _ string) ([]*net.MX, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					},
				}, nil)
				c := timingConnection(t, r)
				f, _ := mxwire.JSONFrame(mxwire.FrameDomainAuth, 0, 1, mxwire.DomainAuth{Domain: "example.test", KeyID: "key1"})
				if err := r.auth(c, f); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if phase == "late response" {
					time.Sleep(mxwire.AuthChallengeTTL + time.Second)
				}
				answerTimingChallenge(t, r, c, priv)
				if phase == "verification timeout" {
					time.Sleep(mxwire.AuthVerifyTimeout + time.Second)
					synctest.Wait()
				}
				result, err := timingFrame(t, c)
				var rejected mxwire.AuthResult
				if err != nil || mxwire.DecodeFrame(result, &rejected) != nil || rejected.Accepted {
					t.Fatalf("expected bounded rejection: %+v, %v", rejected, err)
				}
				if _, err := r.lookup("example.test"); err == nil {
					t.Fatal("expired authentication granted a binding")
				}
			})
		})
	}
}
