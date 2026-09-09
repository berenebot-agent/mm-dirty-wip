package transport_test

import (
	"testing"

	"gatehouse-mail/internal/transport"
	_ "gatehouse-mail/internal/transport/brevo"
	_ "gatehouse-mail/internal/transport/mailgun"
	_ "gatehouse-mail/internal/transport/resend"
	_ "gatehouse-mail/internal/transport/smtp"
)

func TestOutboundRegistry(t *testing.T) {
	for _, name := range []string{"mailgun", "smtp", "brevo", "resend"} {
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
	if len(transport.ListOutbound()) != 4 {
		t.Fatalf("list length %d", len(transport.ListOutbound()))
	}
}
