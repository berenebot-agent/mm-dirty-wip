package httpapp_test

import (
	"strings"
	"testing"
)

// providerOptionOrder returns the provider option labels of the receiving
// dialog's <select> in the order they are rendered.
func providerOptionOrder(t *testing.T, dialog string) []string {
	t.Helper()
	start := strings.Index(dialog, `name="provider"`)
	if start < 0 {
		t.Fatalf("provider select not found:\n%s", dialog)
	}
	rest := dialog[start:]
	if end := strings.Index(rest, "</select>"); end >= 0 {
		rest = rest[:end]
	}
	var out []string
	for _, frag := range strings.Split(rest, "<option ")[1:] {
		value := attrValue(frag, "value")
		if value == "" || value == "inherited" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func attrValue(frag, name string) string {
	needle := name + `="`
	i := strings.Index(frag, needle)
	if i < 0 {
		return ""
	}
	frag = frag[i+len(needle):]
	if j := strings.Index(frag, `"`); j >= 0 {
		return frag[:j]
	}
	return ""
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// TestReceivingProviderOrder pins the receiving dropdown order: Antler MX,
// Direct MX and Remote MX lead in that fixed order, and every other provider
// follows alphabetically by display label.
func TestReceivingProviderOrder(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=receiving").Body.String(), "domain-receiving-dialog-"+domain.ID)
	order := providerOptionOrder(t, body)

	want := []string{"dialmx", "mx", "remotemx", "cloudflare", "mailgun", "postmark", "resend", "sendgrid"}
	if len(order) != len(want) {
		t.Fatalf("receiving provider count = %d %v, want %d %v", len(order), order, len(want), want)
	}
	for i, provider := range want {
		if order[i] != provider {
			t.Fatalf("receiving provider order = %v, want %v", order, want)
		}
	}
}

// TestSendingProviderOrder pins the sending dropdown order: SMTP then Direct MX
// lead, and every other provider follows alphabetically by display label.
func TestSendingProviderOrder(t *testing.T) {
	svc, h, u, domain, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	dialog := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+domain.ID+"&kind=sending").Body.String(), "domain-sending-dialog-"+domain.ID)
	order := providerOptionOrder(t, dialog)

	want := []string{"smtp", "mx", "brevo", "mailgun", "resend"}
	if len(order) != len(want) {
		t.Fatalf("sending provider count = %d %v, want %d %v", len(order), order, len(want), want)
	}
	for i, provider := range want {
		if order[i] != provider {
			t.Fatalf("sending provider order = %v, want %v", order, want)
		}
	}
}
