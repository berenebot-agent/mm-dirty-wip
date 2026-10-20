package mailgun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"strings"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

type Config struct {
	APIKey  string `json:"api_key"`
	Domain  string `json:"domain"`
	APIBase string `json:"api_base,omitempty"`
}
type SendRequest struct {
	From                                      string
	To, CC, BCC                               []string
	Subject, Text, HTML, MessageID, InReplyTo string
	References                                []string
	Attachments                               []transport.OutboundAttachment
}
type SendResult struct {
	ProviderMessageID string `json:"provider_message_id"`
}

type outboundTransport struct{}

func init() { transport.RegisterOutbound(outboundTransport{}) }

func (outboundTransport) Name() string        { return "mailgun" }
func (outboundTransport) Description() string { return "Mailgun API" }
func (outboundTransport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "api_key", Label: "API key", Type: "password", Required: true, Secret: true, Placeholder: "key-..."},
		{Name: "domain", Label: "Sending domain", Type: "text", Required: true, Placeholder: "mg.example.com"},
		{Name: "api_base", Label: "API base URL", Type: "text", Placeholder: "https://api.mailgun.net"},
	}
}
func (outboundTransport) Send(ctx context.Context, cfg map[string]any, m transport.OutboundMessage) (transport.OutboundResult, error) {
	var c Config
	if err := transport.DecodeOutboundConfig(cfg, &c); err != nil {
		return transport.OutboundResult{}, err
	}
	from := m.FromAddress
	if m.FromName != "" {
		from = (&mail.Address{Name: m.FromName, Address: m.FromAddress}).String()
	}
	res, err := Send(ctx, c, SendRequest{From: from, To: m.To, CC: m.CC, BCC: m.BCC, Subject: m.Subject, Text: m.Text, HTML: m.HTML, MessageID: m.MessageID, InReplyTo: m.InReplyTo, References: m.References, Attachments: m.Attachments})
	if err != nil {
		return transport.OutboundResult{}, err
	}
	return transport.OutboundResult{ProviderMessageID: res.ProviderMessageID}, nil
}

func Send(ctx context.Context, c Config, m SendRequest) (SendResult, error) {
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = "https://api.mailgun.net"
	}
	if err := netutil.ValidateBaseURL(base); err != nil {
		return SendResult{}, err
	}
	if c.APIKey == "" || c.Domain == "" {
		return SendResult{}, fmt.Errorf("mailgun api_key and domain are required")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	add := func(key, value string) error { return mw.WriteField(key, value) }
	if err := add("from", m.From); err != nil {
		return SendResult{}, err
	}
	for _, x := range m.To {
		if err := add("to", x); err != nil {
			return SendResult{}, err
		}
	}
	for _, x := range m.CC {
		if err := add("cc", x); err != nil {
			return SendResult{}, err
		}
	}
	for _, x := range m.BCC {
		if err := add("bcc", x); err != nil {
			return SendResult{}, err
		}
	}
	for key, value := range map[string]string{"subject": m.Subject, "text": m.Text} {
		if err := add(key, value); err != nil {
			return SendResult{}, err
		}
	}
	if m.HTML != "" {
		if err := add("html", m.HTML); err != nil {
			return SendResult{}, err
		}
	}
	if m.MessageID != "" {
		if err := add("h:Message-Id", m.MessageID); err != nil {
			return SendResult{}, err
		}
	}
	if m.InReplyTo != "" {
		if err := add("h:In-Reply-To", m.InReplyTo); err != nil {
			return SendResult{}, err
		}
	}
	if len(m.References) > 0 {
		if err := add("h:References", strings.Join(m.References, " ")); err != nil {
			return SendResult{}, err
		}
	}
	for _, attachment := range m.Attachments {
		part, err := mw.CreateFormFile("attachment", attachment.Filename)
		if err != nil {
			return SendResult{}, err
		}
		if _, err = part.Write(attachment.Content); err != nil {
			return SendResult{}, err
		}
	}
	if err := mw.Close(); err != nil {
		return SendResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v3/"+url.PathEscape(c.Domain)+"/messages", &body)
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetBasicAuth("api", c.APIKey)
	resp, err := netutil.HTTPClient().Do(req)
	if err != nil {
		// Mailgun exposes no idempotency key, so a client timeout while awaiting
		// headers leaves an unknown outcome; mark it ambiguous so the outbox
		// does not retry and risk a duplicate delivery.
		if netutil.IsClientTimeout(err) {
			return SendResult{}, &transport.AmbiguousError{Err: err}
		}
		return SendResult{}, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := transport.ProviderError("mailgun", resp.Status, responseBody)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return SendResult{}, &transport.PermanentError{Err: err}
		}
		return SendResult{}, err
	}
	var payload struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(responseBody, &payload)
	return SendResult{ProviderMessageID: payload.ID}, nil
}
