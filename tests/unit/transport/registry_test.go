package transport_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
	_ "github.com/dellarb/mailmoose/internal/transport/brevo"
	_ "github.com/dellarb/mailmoose/internal/transport/mailgun"
	_ "github.com/dellarb/mailmoose/internal/transport/resend"
	_ "github.com/dellarb/mailmoose/internal/transport/smtp"
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
