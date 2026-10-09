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
	if view.TXTValue != "" {
		view.DNS = append(view.DNS, s.dns.checkTXT(domainName, keyID, publicKey, view.TXTValue))
	}
	if len(view.MX) > 0 {
		view.DNS = append(view.DNS, s.dns.checkMX(domainName, view.MX))
	}
	return view
}

// dialMXHealth aggregates a domain's receiver statuses into the single traffic
// light shown on the dashboard. Inbound mail is delivered as long as one
// receiver holds a current authorization, so the light is green when any
// receiver is ready and unexpired, red when every receiver has settled into a
// failure, and amber (pending) while at least one receiver is still working —
// connecting, deferred, or waiting for its first status. Amber is the honest
// "not yet" state at startup and during a reconnect: a red light would cry
// failure while the core is still authorizing. The light answers the one
// question an operator needs at a glance — will mail arrive? — rather than
// enumerating each receiver; the receiving dialog carries the per-connector
// detail. light is a "dns-light" token ("ok", "amber" or "danger") and title is
// the human explanation for its tooltip.
func dialMXHealth(statuses []mxdialStatusView, now time.Time) (light, title string) {
	ready := 0
	pending := 0
	for _, st := range statuses {
		switch {
		case st.State == mxdial.StatusActive && (st.ExpiresAt == nil || st.ExpiresAt.After(now)):
			ready++
		case st.State == mxdial.StatusConnecting, st.State == mxdial.StatusDeferred, st.State == mxdial.StatusDisconnected, st.State == "":
			pending++
		}
	}
	if ready > 0 {
		return "ok", fmt.Sprintf("%d of %d inbound connectors ready — mail will be delivered", ready, len(statuses))
	}
	if pending > 0 {
		return "amber", "Inbound connectors are still connecting — not ready yet"
	}
	if len(statuses) == 0 {
		return "amber", "Waiting for inbound connector status"
	}
	return "danger", "No inbound connector is ready — mail delivery is blocked until a receiver reconnects"
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
