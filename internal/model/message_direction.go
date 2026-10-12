package model

const (
	DirectionInbound  = "inbound"
	DirectionOutbound = "outbound"
	DirectionUnknown  = "unknown"
)

// MessageDirection applies the common mailbox classification policy. Recorded
// direction wins; drafts are otherwise unknown; Sent evidence wins over Inbox.
// Sender addresses and human-readable folder names are never evidence.
func MessageDirection(recorded string, sent, inbox, draft bool) string {
	if recorded == DirectionInbound || recorded == DirectionOutbound {
		return recorded
	}
	if draft {
		return DirectionUnknown
	}
	if sent {
		return DirectionOutbound
	}
	if inbox {
		return DirectionInbound
	}
	return DirectionUnknown
}

// DefaultReplyRecipients refuses to invent recipients for an unknown direction.
func DefaultReplyRecipients(m Message) []string {
	switch m.Direction {
	case DirectionInbound:
		if m.From.Address != "" {
			return []string{m.From.Address}
		}
	case DirectionOutbound:
		return append([]string(nil), m.To...)
	}
	return nil
}
