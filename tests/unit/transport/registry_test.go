package transport_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
	_ "github.com/dellarb/mailmoose/internal/transport/brevo"
	_ "github.com/dellarb/mailmoose/internal/transport/mailgun"
	_ "github.com/dellarb/mailmoose/internal/transport/mx"
	_ "github.com/dellarb/mailmoose/internal/transport/mxdial"
	_ "github.com/dellarb/mailmoose/internal/transport/remotemx"
	_ "github.com/dellarb/mailmoose/internal/transport/resend"
	_ "github.com/dellarb/mailmoose/internal/transport/smtp"
)

// TestInboundMXProviderLabel pins the human-facing label for the Direct MX
// receiving provider, which the per-domain receiving dialog renders verbatim.
func TestInboundMXProviderLabel(t *testing.T) {
	provider, ok := transport.LookupInbound("mx")
	if !ok {
		t.Fatal("mx inbound provider not registered")
	}
	if got, want := provider.Description(), "Direct MX (SMTP to the MailMoose receiver)"; got != want {
		t.Fatalf("mx description = %q, want %q", got, want)
	}
	fields := provider.ConfigFields()
	if len(fields) != 1 || fields[0].Name != "enforcement" {
		t.Fatalf("mx per-domain config = %+v, want only enforcement", fields)
	}
}

// TestInboundRemoteMXProvider pins the Remote MX provider: it has no
// per-domain schema (the receiver is configured once per account) and a picker
// label that names it as the account's own receiver.
func TestInboundRemoteMXProvider(t *testing.T) {
	provider, ok := transport.LookupInbound("remotemx")
	if !ok {
		t.Fatal("remotemx inbound provider not registered")
	}
	if provider.Description() != "Remote MX" {
		t.Fatalf("remotemx description = %q", provider.Description())
	}
	if got := transport.InboundSelectLabel(provider); got != "Remote MailMoose MX (your own receiver)" {
		t.Fatalf("remotemx select label = %q", got)
	}
	if fields := provider.ConfigFields(); len(fields) != 0 {
		t.Fatalf("remotemx per-domain config = %+v, want none", fields)
	}
}

func TestOutboundRegistry(t *testing.T) {
	for _, name := range []string{"mailgun", "smtp", "brevo", "resend", "mx"} {
		provider, ok := transport.LookupOutbound(name)
		if !ok {
			t.Fatalf("provider %q not registered", name)
		}
		if provider.Name() != name || provider.Description() == "" {
			t.Fatalf("bad provider %q: %q / %q", name, provider.Name(), provider.Description())
		}
	}
	if _, ok := transport.LookupOutbound("nope"); ok {
		t.Fatal("unknown provider resolved")
	}
	if len(transport.ListOutbound()) != 5 {
		t.Fatalf("list length %d", len(transport.ListOutbound()))
	}
}
