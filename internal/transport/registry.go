package transport

import "sort"

var inbound = map[string]InboundTransport{}

func RegisterInbound(t InboundTransport) {
	if t == nil || t.Name() == "" {
		return
	}
	inbound[t.Name()] = t
}

func LookupInbound(name string) (InboundTransport, bool) {
	t, ok := inbound[name]
	return t, ok
}

var outbound = map[string]OutboundTransport{}

func RegisterOutbound(t OutboundTransport) {
	if t == nil || t.Name() == "" {
		return
	}
	outbound[t.Name()] = t
}

func LookupOutbound(name string) (OutboundTransport, bool) {
	t, ok := outbound[name]
	return t, ok
}

func ListOutbound() []OutboundTransport {
	out := make([]OutboundTransport, 0, len(outbound))
	for _, t := range outbound {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
