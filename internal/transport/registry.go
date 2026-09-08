package transport

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
