package httpapp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/mxwire"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

// dialMXLiveView is the shared live picture of one domain's Dial MX setup: the
// receiver snapshot, the live per-receiver authentication state the core has
// learned over its outbound sessions, and the published-record checks. The API
// and the setup dialog render the same values so they cannot disagree.
type dialMXLiveView struct {
	Service      string
	ContactEmail string
	Enforcement  string
	TXTName      string
	TXTValue     string
	MX           []dialMXMXInstruction
	Statuses     []mxdialStatusView
	DNS          []domainDNSView
	// Manifest is where the domain's snapshotted receiver set came from, so the
	// setup dialog can say whether it used the live GitHub manifest or the
	// built-in defaults. Nil for a config saved before the record existed.
	Manifest *antlerManifestView
}

// antlerManifestView records where a snapshotted Tusk MX receiver set came
// from. At is the manifest fetch time (RFC3339) and is empty for the built-in
// defaults. It carries no secret.
type antlerManifestView struct {
	Source string `json:"source"`
	At     string `json:"at,omitempty"`
}

// antlerManifestViewFromConfig reads the recorded manifest source off a
// decrypted receiving config. A config with no record (one saved before this
// field existed) returns nil rather than inventing a source.
func antlerManifestViewFromConfig(values map[string]any) *antlerManifestView {
	source, _ := values["receiver_manifest_source"].(string)
	if source == "" {
		return nil
	}
	at, _ := values["receiver_manifest_at"].(string)
	return &antlerManifestView{Source: source, At: at}
}

// dialMXLiveView builds the view for a domain whose effective receiving
// provider is Dial MX. It never returns an error: a missing credential or
// config yields a partial view so the setup dialog still renders.
//
// The published-record checks (DNS) are deliberately not run here: the saved
// status page renders the receivers' self-reported state only, and running a
// resolver lookup on every status poll is wasted work. Callers that need the
// checks call dialMXDNS afterwards (the setup wizard, and the dashboard light).
func (s *Server) dialMXLiveView(domainName, keyID string, publicKey []byte, cfg store.DomainReceivingConfig) dialMXLiveView {
	view := dialMXLiveView{TXTName: "_mail-proof." + domainName}
	values := map[string]any{}
	if dec, err := s.Service.DecryptDomainReceivingConfig(cfg); err == nil {
		values = dec
	}
	if keyID != "" && len(publicKey) > 0 {
		view.TXTValue = mxwire.DomainTXT(keyID, publicKey)
	}
	if dialMXService(values) == mxdial.ServiceHosted {
		view.Service = mxdial.ServiceHosted
		view.ContactEmail, _ = values["contact_email"].(string)
		view.Enforcement, _ = values["enforcement"].(string)
		view.Manifest = antlerManifestViewFromConfig(values)
		for _, r := range app.AntlerReceiversFromConfig(values) {
			view.MX = append(view.MX, dialMXMXInstruction{Hostname: r.SMTPHostname, Priority: r.MXPriority})
		}
	} else {
		view.Service = mxdial.ServiceCustom
	}
	if s.Service.DialMX != nil {
		antlerReceivers := app.AntlerReceiversFromConfig(values)
		for _, st := range s.Service.DialMX.Status(domainName) {
			// A rotation can precede the manager's next reconciliation. Never
			// present authorization for the previous key as current readiness.
			if st.KeyID != "" && st.KeyID != keyID {
				continue
			}
			hostname := st.SMTPHostname
			if view.Service == mxdial.ServiceHosted {
				configured := false
				for _, receiver := range antlerReceivers {
					if receiver.SessionURL == st.ReceiverURL {
						configured = true
						// A receiver the core cannot reach reports no advertised
						// hostname. Fall back to the configured SMTP hostname so
						// the connector row still resolves to its real state
						// (red) instead of an amber "waiting" that hides the
						// failure.
						if hostname == "" {
							hostname = receiver.SMTPHostname
						}
						break
					}
				}
				if !configured {
					continue
				}
			}
			v := mxdialStatusView{ReceiverURL: st.ReceiverURL, State: st.State, Reason: boundStatusReason(st.Reason), SMTPHostname: hostname, HTTPCode: st.HTTPCode}
			if !st.ExpiresAt.IsZero() {
				t := st.ExpiresAt
				v.ExpiresAt = &t
			}
			view.Statuses = append(view.Statuses, v)
		}
	}
	// Include receivers that have not reported a domain status yet, so setup
	// always shows the complete saved Tusk receiver set.
	if view.Service == mxdial.ServiceHosted {
		for _, receiver := range app.AntlerReceiversFromConfig(values) {
			found := false
			for _, status := range view.Statuses {
				if status.ReceiverURL == receiver.SessionURL {
					found = true
					break
				}
			}
			if !found {
				view.Statuses = append(view.Statuses, mxdialStatusView{ReceiverURL: receiver.SessionURL, SMTPHostname: receiver.SMTPHostname, State: mxdial.StatusConnecting})
			}
		}
	} else if raw, ok := values["receiver_urls"].(string); ok {
		for _, receiverURL := range strings.Split(raw, ",") {
			receiverURL = strings.TrimSpace(receiverURL)
			if receiverURL == "" {
				continue
			}
			found := false
			for _, status := range view.Statuses {
				if status.ReceiverURL == receiverURL {
					found = true
					break
				}
			}
			if !found {
				view.Statuses = append(view.Statuses, mxdialStatusView{ReceiverURL: receiverURL, State: mxdial.StatusConnecting})
			}
		}
	}
	return view
}

// dialMXDNS returns the domain's published-record checks from the watcher's
// single shared verdict. It runs no resolver lookup of its own: reading asks the
// watcher to re-check when the cached verdict has gone stale (RequestCheckIfStale,
// coalesced into the watcher's own loop), and otherwise returns the in-memory
// value, so the wizard, the status view and the dashboard light all read the same
// picture and no render blocks on DNS.
func (s *Server) dialMXDNS(domainID string) []domainDNSView {
	if s.Service.DNS != nil {
		s.Service.DNS.RequestCheckIfStale(domainID)
	}
	verdict, _ := s.Service.DNSState(domainID)
	return dnsViewsFromVerdict(verdict)
}

// dialMXExtraMX returns a domain's "extra MX" verdict: the published MX
// hostnames that are not one of its configured Tusk receivers. It reads the
// single shared verdict owned by the DNS watcher, so the dashboard light and
// the status dialog always agree and neither runs its own DNS lookup on a render.
// A domain the watcher has not resolved yet reads as an empty verdict.
func (s *Server) dialMXExtraMX(domainID string) []string {
	if s.Service.DNS != nil {
		s.Service.DNS.RequestCheckIfStale(domainID)
	}
	hosts, _ := s.Service.ExtraMXState(domainID)
	return hosts
}

// dialMXHealth aggregates a domain's receiver statuses into the single traffic
// light shown on the dashboard. It is: green when at least one connector row is
// fully green and no unexpected MX records are published; amber when at least
// one connector is green but the domain's MX records also name servers that are
// not Tusk receivers (mail is delivered, but routing is ambiguous and may not
// go via Tusk); red otherwise. A receiver reports "ready" only after it has
// proved BOTH domain authority (its _mail-proof TXT key) and routing (its own
// SMTP hostname is published in the domain's MX records), so a ready row already
// means mail will be delivered and the aggregate needs no separate MX check
// beyond the extra-record warning. A receiver that is not routed reports
// rejected/not_mx and is not green.
//
// A receiver that has disconnected, been rejected, or become unreachable is not
// green: green means mail will arrive now, and a reconnecting receiver is not
// yet delivering. The receiving dialog carries the per-connector detail. light
// is a "dns-light" token ("ok", "amber" or "danger") and title is the human
// explanation for its tooltip.
//
// A ready sibling must not hide a replaced binding: another connection owns it.
func dialMXHealth(statuses []mxdialStatusView, unexpectedMX []string, now time.Time) (light, title string) {
	for _, st := range statuses {
		if st.Reason == "replaced" {
			return "danger", "Another connection took over this receiver's domain binding — check for another Mammoth Mail instance using the same domain key"
		}
	}
	ready := 0
	for _, st := range statuses {
		if st.State != mxdial.StatusActive || (st.ExpiresAt != nil && !st.ExpiresAt.After(now)) {
			continue
		}
		ready++
	}
	if ready == 0 {
		return "danger", "No inbound connector is ready — mail delivery is blocked until a receiver authorizes and your MX points at it"
	}
	if len(unexpectedMX) > 0 {
		return "amber", fmt.Sprintf("%d of %d inbound connectors ready, but extra MX records exist (%s) — mail may not be delivered via Tusk", ready, len(statuses), strings.Join(unexpectedMX, ", "))
	}
	return "ok", fmt.Sprintf("%d of %d inbound connectors ready — mail will be delivered", ready, len(statuses))
}

// boundStatusReason truncates a receiver-supplied reason so an unexpectedly
// long or hostile value cannot distort an API response or the dialog.
func boundStatusReason(reason string) string {
	if len(reason) > 120 {
		return reason[:120]
	}
	return reason
}

// instructionView maps the live view onto the API instruction shape.
func (v dialMXLiveView) instructionView() *dialMXDNSInstructions {
	if v.TXTValue == "" && len(v.MX) == 0 {
		return nil
	}
	out := &dialMXDNSInstructions{
		Service:     v.Service,
		Contact:     v.ContactEmail,
		Enforcement: v.Enforcement,
		TXTName:     v.TXTName,
		TXTValue:    v.TXTValue,
		MX:          v.MX,
		Receiver:    v.Statuses,
	}
	return out
}

// statusExpiry renders the remaining authorization window compactly for the
// setup dialog, or "" when the receiver issued no bounded expiry.
func statusExpiry(v mxdialStatusView) string {
	if v.ExpiresAt == nil {
		return ""
	}
	d := time.Until(*v.ExpiresAt)
	if d <= 0 {
		return ""
	}
	if d >= time.Hour {
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

// dialMXHasReceiverTargets reports whether a domain's Dial MX receiving
// configuration actually names something to deliver to: at least one Tusk
// receiver, or (for a custom service) at least one receiver URL. It is the gate
// for the dashboard's aggregate inbound light. A Dial MX domain whose Tusk
// receiver set is empty — a setup that was started and abandoned, or a manifest
// that advertised no receivers — has nothing to deliver to and nothing to fix,
// so it must not be painted red "mail delivery is blocked" for a setup that was
// never finished. A missing or undecryptable configuration is likewise no target.
//
// A custom-service config always carries at least one receiver URL (the save path
// rejects an empty set), so in practice this reduces to "the Tusk receiver set
// is not empty", but it is expressed generally so a future custom shape cannot
// slip a phantom light through.
func (s *Server) dialMXHasReceiverTargets(ctx context.Context, accountID, domainID string) bool {
	cfg, err := s.Service.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, "dialmx")
	if err != nil {
		return false
	}
	values, err := s.Service.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return false
	}
	if len(app.AntlerReceiversFromConfig(values)) > 0 {
		return true
	}
	raw, _ := values["receiver_urls"].(string)
	return strings.TrimSpace(raw) != ""
}

// dialMXCustomService reports whether a domain's stored Dial MX receiving config
// uses custom receiver URLs rather than the hosted Tusk MX relay. A missing or
// undecryptable config is not custom.
func (s *Server) dialMXCustomService(ctx context.Context, accountID, domainID string) bool {
	cfg, err := s.Service.Store.ResolveDomainReceivingConfig(ctx, accountID, domainID, "dialmx")
	if err != nil {
		return false
	}
	values, err := s.Service.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return false
	}
	return dialMXService(values) == mxdial.ServiceCustom
}

// dialMXService classifies a decrypted Dial MX receiving config as the hosted
// Tusk MX relay or a custom receiver set. The two spellings live here so the
// live view and the dashboard label cannot disagree about which one a domain is.
func dialMXService(values map[string]any) string {
	if service, _ := values["service"].(string); service == mxdial.ServiceHosted {
		return mxdial.ServiceHosted
	}
	return mxdial.ServiceCustom
}

// receivingCellLabel returns the label the dashboard's Receiving cell shows for a
// domain whose receiving configuration is its own, so the live snapshot can
// rewrite a cell whose row was rendered before the configuration existed. It is
// empty for a domain with no receiving provider and for one that inherits its
// receiving config: an inherited cell reads "(inherited)" and its dialog cannot
// change the provider, so the snapshot leaves it to the server-rendered row.
func (s *Server) receivingCellLabel(ctx context.Context, d model.Domain) string {
	if d.ReceivingProvider == "" || d.ReceivingInheritedFrom != "" {
		return ""
	}
	custom := false
	if normalizeDomainProvider(d.ReceivingProvider) == "dialmx" {
		custom = s.dialMXCustomService(ctx, d.AccountID, d.ID)
	}
	return receivingProviderLabel(d.ReceivingProvider, custom)
}
