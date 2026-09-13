package mxagent

import (
	"context"
	"io"
	"net"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"gatehouse-mail/internal/mxwire"
)

// Verifier computes SPF/DKIM/DMARC evidence for one message. It never trusts
// incoming Authentication-Results/Received-SPF/Received/Return-Path headers:
// the caller supplies the actual peer IP, HELO and MAIL FROM observed at the
// SMTP edge. Verification runs on the original bytes before any local trace or
// authentication header is added.
type Verifier struct {
	cfg   Config
	spfNS spf.DNSResolver
}

func NewVerifier(cfg Config) *Verifier {
	v := &Verifier{cfg: cfg}
	if cfg.DNSResolver != "" {
		v.spfNS = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: cfg.DNSTimeout}
				return d.DialContext(ctx, network, cfg.DNSResolver)
			},
		}
	}
	return v
}

// Verify computes the evidence. A nil/empty result is "no evidence": it must
// never be turned into a definitive failure. raw streams the original bytes and
// is rewound before each verification pass, so a large message is never held in
// memory.
func (v *Verifier) Verify(ctx context.Context, raw io.ReadSeeker, peerIP net.IP, helo, mailFrom, fromDomain string) mxwire.AuthResults {
	var out mxwire.AuthResults
	out.Source = "edge"
	out.Evaluator = "gatehouse-mx/1"
	lookupTXT := v.txtLookup

	if v.cfg.VerifySPF {
		out.SPF = v.verifySPF(ctx, peerIP, helo, mailFrom)
	}
	if v.cfg.VerifyDKIM {
		out.DKIM = v.verifyDKIM(ctx, raw, fromDomain, lookupTXT)
	}
	if v.cfg.VerifyDMARC {
		out.DMARC = v.verifyDMARC(ctx, fromDomain, out.SPF, out.DKIM, lookupTXT)
	}
	return out
}

func (v *Verifier) txtLookup(domain string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), v.cfg.DNSTimeout)
	defer cancel()
	resolver := v.spfNS
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return resolver.LookupTXT(ctx, strings.TrimSuffix(domain, "."))
}

// verifySPF evaluates RFC 7208 against the out-of-band peer identity. A null
// MAIL FROM falls back to the HELO identity. The library enforces the 10-lookup
// and 2-void-lookup budgets itself.
func (v *Verifier) verifySPF(ctx context.Context, peerIP net.IP, helo, mailFrom string) *mxwire.SPFEvidence {
	if peerIP == nil {
		return &mxwire.SPFEvidence{Result: "none", Reason: "no peer ip"}
	}
	opts := []spf.Option{spf.WithContext(ctx)}
	if v.spfNS != nil {
		opts = append(opts, spf.WithResolver(v.spfNS))
	}
	sender := mailFrom
	if strings.TrimSpace(sender) == "" {
		sender = "postmaster@" + strings.TrimSuffix(helo, ".")
	}
	res, err := spf.CheckHostWithSender(peerIP, strings.TrimSuffix(helo, "."), sender, opts...)
	ev := &mxwire.SPFEvidence{Result: strings.ToLower(string(res))}
	if err != nil && (res == "" || res == spf.None) {
		ev.Result = "temperror"
		ev.Reason = "spf evaluation error"
	}
	if at := strings.LastIndex(sender, "@"); at >= 0 {
		ev.Domain = strings.ToLower(sender[at+1:])
	}
	ev.Scope = "mailfrom"
	if strings.TrimSpace(mailFrom) == "" {
		ev.Scope = "helo"
		ev.Domain = strings.ToLower(strings.TrimSuffix(helo, "."))
	}
	return ev
}

// verifyDKIM verifies every signature on the original bytes, capped at
// MaxDKIMSignatures. A valid but unaligned signature remains pass with
// aligned=false; it is never reported as fail. raw is read from the start and
// rewound first so it can be reused.
func (v *Verifier) verifyDKIM(ctx context.Context, raw io.ReadSeeker, fromDomain string, lookupTXT func(string) ([]string, error)) []mxwire.DKIMEvidence {
	if _, err := raw.Seek(0, io.SeekStart); err != nil {
		return []mxwire.DKIMEvidence{{Result: "temperror", Error: "rewind"}}
	}
	verifs, err := dkim.VerifyWithOptions(raw, &dkim.VerifyOptions{
		MaxVerifications: mxwire.MaxDKIMSignatures,
		LookupTXT:        lookupTXT,
	})
	if err != nil && verifs == nil {
		return []mxwire.DKIMEvidence{{Result: "temperror", Error: classifyDKIMErr(err)}}
	}
	out := make([]mxwire.DKIMEvidence, 0, len(verifs))
	for _, sig := range verifs {
		ev := mxwire.DKIMEvidence{Domain: strings.ToLower(strings.TrimSpace(sig.Domain)), Selector: ""}
		if sig.Err == nil {
			ev.Result = "pass"
			ev.Aligned = DomainsAlign(fromDomain, sig.Domain, false)
		} else {
			ev.Result = dkimResult(sig.Err)
			ev.Error = classifyDKIMErr(sig.Err)
		}
		out = append(out, ev)
	}
	return out
}

func dkimResult(err error) string {
	switch {
	case err == nil:
		return "pass"
	case dkim.IsTempFail(err):
		return "temperror"
	case dkim.IsPermFail(err):
		return "permerror"
	default:
		return "fail"
	}
}

// classifyDKIMErr returns a bounded diagnostic string, never raw key material.
func classifyDKIMErr(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case dkim.IsTempFail(err):
		return "temperror"
	case dkim.IsPermFail(err):
		return "permerror"
	default:
		return "signature_failed"
	}
}

// verifyDMARC discovers the organizational-domain policy and evaluates
// alignment itself (go-msgauth only looks up and parses the record). A
// published p=none is preserved as the policy; the result is derived from the
// SPF/DKIM alignment.
func (v *Verifier) verifyDMARC(ctx context.Context, fromDomain string, spfEv *mxwire.SPFEvidence, dkimEvs []mxwire.DKIMEvidence, lookupTXT func(string) ([]string, error)) *mxwire.DMARCEvidence {
	fromDomain = strings.ToLower(strings.TrimSpace(fromDomain))
	if fromDomain == "" {
		return &mxwire.DMARCEvidence{Result: "none", Reason: "no from domain"}
	}
	ev := &mxwire.DMARCEvidence{FromDomain: fromDomain}
	rec, orgDomain, err := lookupDMARCPolicy(fromDomain, lookupTXT)
	ev.PolicyDomain = orgDomain
	if err != nil {
		if err == dmarc.ErrNoPolicy {
			ev.Result = "none"
			ev.Reason = "no policy"
		} else if dmarc.IsTempFail(err) {
			ev.Result = "temperror"
			ev.Reason = "dns"
		} else {
			ev.Result = "permerror"
			ev.Reason = "record"
		}
		return ev
	}
	ev.Policy = string(rec.Policy)
	strict := rec.SPFAlignment == dmarc.AlignmentStrict
	spfAligned := spfEv != nil && strings.EqualFold(spfEv.Result, "pass") && DomainsAlign(fromDomain, spfEv.Domain, strict)
	dkimAligned := false
	for _, d := range dkimEvs {
		if strings.EqualFold(d.Result, "pass") && DomainsAlign(fromDomain, d.Domain, rec.DKIMAlignment == dmarc.AlignmentStrict) {
			dkimAligned = true
			break
		}
	}
	ev.SPFAligned = spfAligned
	ev.DKIMAligned = dkimAligned
	if spfAligned || dkimAligned {
		ev.Result = "pass"
	} else {
		ev.Result = "fail"
	}
	return ev
}

// lookupDMARCPolicy finds the DMARC record for domain or its organizational
// domain. It uses the provided TXT lookup and returns the discovered record's
// domain.
func lookupDMARCPolicy(domain string, lookupTXT func(string) ([]string, error)) (*dmarc.Record, string, error) {
	candidates := []string{domain}
	if org := OrganizationalDomain(domain); org != "" && org != domain {
		candidates = append(candidates, org)
	}
	var lastErr error
	for _, cand := range candidates {
		txts, err := lookupTXT("_dmarc." + cand)
		if err != nil {
			lastErr = err
			continue
		}
		for _, txt := range txts {
			if !strings.HasPrefix(txt, "v=") {
				continue
			}
			rec, perr := dmarc.Parse(txt)
			if perr == dmarc.ErrNoPolicy {
				continue
			}
			if perr != nil {
				lastErr = perr
				continue
			}
			return rec, cand, nil
		}
	}
	if lastErr != nil {
		return nil, candidates[len(candidates)-1], lastErr
	}
	return nil, candidates[len(candidates)-1], dmarc.ErrNoPolicy
}

// DomainsAlign reports whether a checked domain aligns with the From domain.
// Relaxed alignment compares organizational domains; strict requires an exact
// match.
func DomainsAlign(fromDomain, checked string, strict bool) bool {
	from := strings.ToLower(strings.TrimSpace(fromDomain))
	got := strings.ToLower(strings.TrimSpace(checked))
	if from == "" || got == "" {
		return false
	}
	if strict {
		return from == got
	}
	return OrganizationalDomain(from) == OrganizationalDomain(got)
}

// OrganizationalDomain returns the registrable organization domain for DMARC
// relaxed alignment, using the embedded Public Suffix List so multi-label
// suffixes (*.co.uk), wildcard and private suffixes (*.github.io,
// *.herokuapp.com) and IDN forms are handled correctly rather than by a small
// hand-maintained approximation. It never authorizes a decision on its own; it
// only aligns the checked and From domains for local spam scoring.
//
// A domain that is itself a public suffix (or otherwise has no registrable
// label) is returned unchanged, so alignment compares it exactly and two
// tenants of the same hosted suffix are never treated as one organization.
func OrganizationalDomain(domain string) string {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return ""
	}
	// EffectiveTLDPlusOne returns an error for a public suffix or a lone
	// label; fall back to the input so alignment stays strict rather than
	// over-broad.
	org, err := publicsuffix.EffectiveTLDPlusOne(domain)
	if err != nil {
		return domain
	}
	return org
}
