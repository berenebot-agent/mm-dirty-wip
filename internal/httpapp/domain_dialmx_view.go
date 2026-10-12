package httpapp

import (
	"fmt"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// dialMXLiveView is the shared live picture of one domain's Dial MX setup: the
// receiver snapshot, the live per-receiver authentication state the core has
// learned over its outbound sessions, and the published-record checks. The API
// and the setup dialog render the same values so they cannot disagree.
type dialMXLiveView struct {
	Service      string
	ContactEmail string
	TXTName      string
	TXTValue     string
	MX           []dialMXMXInstruction
	Statuses     []mxdialStatusView
	DNS          []domainDNSView
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
	view := dialMXLiveView{TXTName: "_mailmoose-mx." + domainName}
	values := map[string]any{}
	if dec, err := s.Service.DecryptDomainReceivingConfig(cfg); err == nil {
		values = dec
	}
	if keyID != "" && len(publicKey) > 0 {
		view.TXTValue = mxwire.DomainTXT(keyID, publicKey)
	}
	if service, _ := values["service"].(string); service == mxdial.ServiceAntler {
		view.Service = mxdial.ServiceAntler
		view.ContactEmail, _ = values["contact_email"].(string)
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
			if view.Service == mxdial.ServiceAntler {
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
			v := mxdialStatusView{ReceiverURL: st.ReceiverURL, State: st.State, Reason: boundStatusReason(st.Reason), SMTPHostname: hostname}
			if !st.ExpiresAt.IsZero() {
				t := st.ExpiresAt
				v.ExpiresAt = &t
			}
			view.Statuses = append(view.Statuses, v)
		}
	}
	// Include receivers that have not reported a domain status yet, so setup
	// always shows the complete saved Antler receiver set.
	if view.Service == mxdial.ServiceAntler {
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
// single shared verdict. It runs no resolver lookup of its own: the watcher
// re-checks on a fixed interval and pushes changes over SSE, so the wizard, the
// status view and the dashboard light all read the same in-memory value.
func (s *Server) dialMXDNS(domainID string) []domainDNSView {
	verdict, _ := s.Service.DNSState(domainID)
	return dnsViewsFromVerdict(verdict)
}

// dialMXExtraMX returns a domain's "extra MX" verdict: the published MX
// hostnames that are not one of its configured Antler receivers. It reads the
// single shared verdict owned by the ExtraMXWatcher (which re-checks on a fixed
// interval and pushes changes over SSE), so the dashboard light and the status
// dialog always agree and neither runs its own DNS lookup on a render. A domain
// the watcher has not resolved yet reads as an empty verdict.
func (s *Server) dialMXExtraMX(domainID string) []string {
	hosts, _ := s.Service.ExtraMXState(domainID)
	return hosts
}

// dialMXHealth aggregates a domain's receiver statuses into the single traffic
// light shown on the dashboard. It is: green when at least one connector row is
// fully green and no unexpected MX records are published; amber when at least
// one connector is green but the domain's MX records also name servers that are
// not Antler receivers (mail is delivered, but routing is ambiguous and may not
// go via Antler); red otherwise. A receiver reports "ready" only after it has
// proved BOTH domain authority (its _mailmoose-mx TXT key) and routing (its own
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
func dialMXHealth(statuses []mxdialStatusView, unexpectedMX []string, now time.Time) (light, title string) {
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
		return "amber", fmt.Sprintf("%d of %d inbound connectors ready, but extra MX records exist (%s) — mail may not be delivered via Antler", ready, len(statuses), strings.Join(unexpectedMX, ", "))
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
		Service:  v.Service,
		Contact:  v.ContactEmail,
		TXTName:  v.TXTName,
		TXTValue: v.TXTValue,
		MX:       v.MX,
		Receiver: v.Statuses,
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
