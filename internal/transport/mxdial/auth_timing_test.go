package mxdial

// Internal access and virtual time pin deadlines and stale-frame handling without
// waiting tens of seconds or exposing production clock/test configuration.
import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

func timingSession(t *testing.T) (*session, <-chan mxwire.Frame) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := Domain{Name: "example.test", KeyID: "key1", PrivateKey: priv}
	m := New(nil, Config{MaxAuthInflight: 1})
	s := m.newSession("https://mx.test", 0, map[string]Domain{d.Name: d})
	s.domains = s.desired
	s.auths = map[string]*auth{}
	s.byChan = map[uint64]*auth{}
	s.authLimit = 1
	s.ready = mxwire.Ready{ReceiverID: "receiver", ConnectionID: "connection"}
	m.owner[statusKey(d.Name, s.url)] = s
	reader, writer := io.Pipe()
	s.writer = writer
	t.Cleanup(func() {
		s.cancel()
		writer.Close()
		reader.Close()
	})
	frames := make(chan mxwire.Frame, 16)
	go func() {
		for {
			f, err := mxwire.ReadFrame(reader)
			if err != nil {
				return
			}
			frames <- f
		}
	}()
	return s, frames
}

func TestAuthenticationWaitsForDNSBudgets(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s, frames := timingSession(t)
		if err := s.authenticateAll(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		first := <-frames
		time.Sleep(11 * time.Second)
		if err := s.authenticateAll(); err != nil {
			t.Fatal(err)
		}
		if s.nextCh != first.ChannelID {
			t.Fatal("retired authentication while initial DNS was in budget")
		}
		challenge := mxwire.Challenge{Domain: "example.test", KeyID: "key1", ReceiverID: s.ready.ReceiverID, ConnectionID: s.ready.ConnectionID, Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}
		f, _ := mxwire.JSONFrame(mxwire.FrameChallenge, 0, first.ChannelID, challenge)
		if err := s.challenge(f); err != nil {
			t.Fatal(err)
		}
		time.Sleep(21 * time.Second)
		if err := s.authenticateAll(); err != nil {
			t.Fatal(err)
		}
		result, _ := mxwire.JSONFrame(mxwire.FrameAuthResult, 0, first.ChannelID, mxwire.AuthResult{Domain: "example.test", KeyID: "key1", Accepted: true, ExpiresAt: time.Now().Add(mxwire.AuthLifetime)})
		if err := s.authResult(result); err != nil {
			t.Fatal(err)
		}
		if st := s.m.Status("example.test"); len(st) != 1 || st[0].State != StatusActive {
			t.Fatalf("delayed valid result not accepted: %+v", st)
		}
	})
}

func TestAuthenticationTimeoutReleasesSlotAndIgnoresLateResult(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s, frames := timingSession(t)
		if err := s.authenticateAll(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		first := <-frames
		time.Sleep(mxwire.AuthLookupTimeout + mxwire.AuthResultGrace + time.Second)
		if err := s.authenticateAll(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		unregister, second := <-frames, <-frames
		if unregister.Type != mxwire.FrameDomainUnregister || unregister.ChannelID != first.ChannelID || second.Type != mxwire.FrameDomainAuth || second.ChannelID <= first.ChannelID {
			t.Fatalf("expected retirement then retry: %+v, %+v", unregister, second)
		}
		if s.authInflight != 1 || len(s.m.authSem) != 1 {
			t.Fatal("retry leaked the only authentication slot")
		}
		late, _ := mxwire.JSONFrame(mxwire.FrameAuthResult, 0, first.ChannelID, mxwire.AuthResult{Domain: "example.test", KeyID: "key1", Accepted: true, ExpiresAt: time.Now().Add(mxwire.AuthLifetime)})
		if err := s.authResult(late); err != nil {
			t.Fatal(err)
		}
		if s.auths["example.test"].state != authPending || s.auths["example.test"].channel != second.ChannelID {
			t.Fatal("late result changed the new authentication")
		}
	})
}
