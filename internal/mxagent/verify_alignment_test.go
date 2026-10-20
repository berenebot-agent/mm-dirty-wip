// In-package exception: deterministic TXT fixtures exercise the unexported
// policy evaluator without public DNS or a test-only production API.
package mxagent

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

func TestSPFAlignmentWithoutDMARCPolicy(t *testing.T) {
	ev := &mxwire.SPFEvidence{Result: "pass", Domain: "mail.example.com"}
	v := &Verifier{}
	dmarc := v.verifyDMARC(context.Background(), "example.com", ev, nil, func(string) ([]string, error) { return nil, nil })
	if dmarc.Result != "none" || !ev.Aligned {
		t.Fatalf("no-policy SPF evidence lost: %+v %+v", dmarc, ev)
	}
	dmarc = v.verifyDMARC(context.Background(), "example.com", ev, nil, func(string) ([]string, error) {
		return []string{"v=DMARC1; p=reject; aspf=s"}, nil
	})
	if dmarc.Result != "fail" || ev.Aligned {
		t.Fatalf("strict policy not authoritative: %+v %+v", dmarc, ev)
	}
}
