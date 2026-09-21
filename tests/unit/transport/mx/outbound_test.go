package mx_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/mx"
)

type resolver struct {
	mx  []*net.MX
	mxe error
	ips map[string][]net.IP
}

func (r resolver) LookupMX(context.Context, string) ([]*net.MX, error) { return r.mx, r.mxe }
func (r resolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	return r.ips[host], nil
}

func TestDirectMXRequiresOneRecipientAndHELO(t *testing.T) {
	base := transport.OutboundMessage{FromAddress: "sender@example.com", To: []string{"a@example.net"}, RawMIME: []byte("Subject: hi\r\n\r\nbody\r\n")}
	for name, msg := range map[string]transport.OutboundMessage{
		"multiple": {FromAddress: base.FromAddress, To: []string{"a@example.net", "b@example.net"}, RawMIME: base.RawMIME},
		"none":     {FromAddress: base.FromAddress, RawMIME: base.RawMIME},
	} {
		t.Run(name, func(t *testing.T) {
			err := mx.Send(context.Background(), mx.OutboundConfig{HELO: "mail.example.com"}, msg, false, resolver{})
			if err == nil {
				t.Fatal("expected recipient validation error")
			}
		})
	}
	if err := mx.Send(context.Background(), mx.OutboundConfig{}, base, false, resolver{}); err == nil || !strings.Contains(err.Error(), "HELO") {
		t.Fatalf("HELO validation error = %v", err)
	}
}

func TestDirectMXTargetsPreferMXAndFallbackToA(t *testing.T) {
	ctx := context.Background()
	targets, err := mx.ResolveTargets(ctx, resolver{mx: []*net.MX{{Host: "low.example.", Pref: 20}, {Host: "high.example.", Pref: 10}}}, "Example.NET")
	if err != nil || strings.Join(targets, ",") != "high.example,low.example" {
		t.Fatalf("targets = %v, err = %v", targets, err)
	}
	targets, err = mx.ResolveTargets(ctx, resolver{mxe: &net.DNSError{IsNotFound: true}}, "example.net")
	if err != nil || strings.Join(targets, ",") != "example.net" {
		t.Fatalf("fallback targets = %v, err = %v", targets, err)
	}
}
