package httpapp

import (
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
		for _, st := range s.Service.DialMX.Status(domainName) {
			if view.Service == mxdial.ServiceAntler {
				configured := false
				for _, receiver := range app.AntlerReceiversFromConfig(values) {
					if receiver.SessionURL == st.ReceiverURL {
						configured = true
						break
					}
				}
				if !configured {
					continue
				}
			}
			v := mxdialStatusView{ReceiverURL: st.ReceiverURL, State: st.State, Reason: boundStatusReason(st.Reason), SMTPHostname: st.SMTPHostname}
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
				view.Statuses = append(view.Statuses, mxdialStatusView{ReceiverURL: receiver.SessionURL, SMTPHostname: receiver.SMTPHostname, State: "connecting"})
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
				view.Statuses = append(view.Statuses, mxdialStatusView{ReceiverURL: receiverURL, State: "connecting"})
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
