package one_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/transport/one"
	"github.com/dellarb/mammothmail/tests/support/onefixture"
)

func TestOneSenderDisplayNameUsesVerifiedMailbox(t *testing.T) {
	f := onefixture.New(t)
	c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
	name, err := c.SenderName(context.Background(), onefixture.Selector, onefixture.Email)
	if err != nil || name != onefixture.Name {
		t.Fatalf("proper sender name: %q %v", name, err)
	}
	name, err = c.SenderName(context.Background(), onefixture.Selector, "unmatched@example.test")
	if err != nil || name != "" {
		t.Fatal("another alias's display name used", name, err)
	}
}

func TestOneSenderNameOptionalAndFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "denied", "empty"} {
		t.Run(mode, func(t *testing.T) {
			f := onefixture.New(t)
			f.Change(func(f *onefixture.Fixture) {
				switch mode {
				case "missing":
					f.Actions = f.Actions[:len(f.Actions)-1]
				case "duplicate":
					f.Actions = append(f.Actions, f.Actions[len(f.Actions)-1])
				case "denied":
					f.Status["GET /v1/passthrough/gmail/v1/users/me/settings/sendAs"] = 403
				case "empty":
					f.SenderNames[onefixture.Email] = ""
				}
			})
			c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
			if err := c.ValidateActions(context.Background()); err != nil {
				t.Fatal("optional naming blocked mailbox compatibility", err)
			}
			name, err := c.SenderName(context.Background(), onefixture.Selector, onefixture.Email)
			if name != "" || (mode != "empty" && err == nil) {
				t.Fatalf("optional lookup: %q %v", name, err)
			}
			if _, err := c.Gmail(onefixture.Selector, nil, nil).Profile(context.Background(), ""); err != nil {
				t.Fatal("name lookup disabled valid mailbox access", err)
			}
		})
	}
}

func TestOneSenderNameFallsBackToSentMetadata(t *testing.T) {
	for _, mode := range []string{"inherited", "missing-action", "wrong-alias", "invalid-header"} {
		t.Run(mode, func(t *testing.T) {
			f := onefixture.New(t)
			f.Change(func(f *onefixture.Fixture) {
				f.SenderNames[onefixture.Email] = ""
				f.SentFrom = `Donna Sterling <` + onefixture.Email + `>`
				if mode == "missing-action" {
					f.Actions = f.Actions[:len(f.Actions)-1]
				}
				if mode == "wrong-alias" {
					f.SentFrom = `Wrong Name <alias@example.test>`
				}
				if mode == "invalid-header" {
					f.SentFrom = `not a valid address`
				}
			})
			c := one.NewClient(onefixture.Key, f.Client(t), &one.Limiter{Interval: time.Nanosecond})
			name, err := c.SenderName(context.Background(), onefixture.Selector, onefixture.Email)
			want := "Donna Sterling"
			if mode == "wrong-alias" || mode == "invalid-header" {
				want = ""
			}
			if err != nil || name != want {
				t.Fatalf("sent name: %q, want %q: %v", name, want, err)
			}
		})
	}
}
