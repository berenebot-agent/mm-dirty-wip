package transport

import (
	"context"
	"encoding/json"
)

type OutboundMessage struct {
	FromName, FromAddress string
	To, CC, BCC           []string
	Subject, Text, HTML   string
	MessageID             string
	InReplyTo             string
	References            []string
	RawMIME               []byte
	Attachments           []OutboundAttachment
}

type OutboundAttachment struct {
	Filename, ContentType string
	Content               []byte
}

type OutboundResult struct{ ProviderMessageID string }

type OutboundTransport interface {
	Name() string
	Description() string
	Send(ctx context.Context, cfg map[string]any, m OutboundMessage) (OutboundResult, error)
}

func DecodeOutboundConfig(in map[string]any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
