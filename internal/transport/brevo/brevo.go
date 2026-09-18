package brevo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

type Config struct {
	APIKey  string `json:"api_key"`
	APIBase string `json:"api_base,omitempty"`
}

type recipient struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type attachment struct {
	Name    string `json:"name"`
	Content []byte `json:"content"`
}

type payload struct {
	Sender      recipient         `json:"sender"`
	To          []recipient       `json:"to"`
	CC          []recipient       `json:"cc,omitempty"`
	BCC         []recipient       `json:"bcc,omitempty"`
	ReplyTo     recipient         `json:"replyTo,omitempty"`
	Subject     string            `json:"subject"`
	TextContent string            `json:"textContent,omitempty"`
	HTMLContent string            `json:"htmlContent,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachment  []attachment      `json:"attachment,omitempty"`
}

type response struct {
	MessageID  string   `json:"messageId"`
	MessageIDs []string `json:"messageIds"`
}

type outboundTransport struct{}

func init() { transport.RegisterOutbound(outboundTransport{}) }

func (outboundTransport) Name() string        { return "brevo" }
func (outboundTransport) Description() string { return "Brevo API" }
func (outboundTransport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "api_key", Label: "API key", Type: "password", Required: true, Secret: true, Placeholder: "xkeysib-..."},
		{Name: "api_base", Label: "API base URL", Type: "text", Placeholder: "https://api.brevo.com"},
	}
}
func (outboundTransport) Send(ctx context.Context, cfg map[string]any, m transport.OutboundMessage) (transport.OutboundResult, error) {
	var c Config
	if err := transport.DecodeOutboundConfig(cfg, &c); err != nil {
		return transport.OutboundResult{}, err
	}
	res, err := Send(ctx, c, m)
	if err != nil {
		return transport.OutboundResult{}, err
	}
	return transport.OutboundResult{ProviderMessageID: res.ProviderMessageID}, nil
}

type SendResult struct {
	ProviderMessageID string `json:"provider_message_id"`
}

func Send(ctx context.Context, c Config, m transport.OutboundMessage) (SendResult, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return SendResult{}, fmt.Errorf("brevo api_key is required")
	}
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = "https://api.brevo.com"
	}
	if err := netutil.ValidateBaseURL(base); err != nil {
		return SendResult{}, err
	}
	p := payload{
		Sender:      recipient{Email: m.FromAddress},
		To:          recipients(m.To),
		CC:          recipients(m.CC),
		BCC:         recipients(m.BCC),
		ReplyTo:     recipient{Email: m.FromAddress},
		Subject:     m.Subject,
		TextContent: m.Text,
		HTMLContent: m.HTML,
	}
	if m.FromName != "" {
		p.Sender.Name = m.FromName
		p.ReplyTo.Name = m.FromName
	}
	// Brevo's API does not support standard email headers: it assigns its own
	// Message-ID and returns it in the response. Supplying Message-ID made
	// Brevo echo our synthesized id back as the response messageId while
	// putting a different id on the wire, so the stored provider_message_id
	// never matched what recipients replied to. Omit Message-ID and let Brevo
	// own it. In-Reply-To/References are standard headers too and are
	// best-effort only.
	if m.InReplyTo != "" || len(m.References) > 0 {
		p.Headers = map[string]string{}
		if m.InReplyTo != "" {
			p.Headers["In-Reply-To"] = m.InReplyTo
		}
		if len(m.References) > 0 {
			p.Headers["References"] = strings.Join(m.References, " ")
		}
	}
	for _, a := range m.Attachments {
		p.Attachment = append(p.Attachment, attachment{Name: a.Filename, Content: a.Content})
	}
	body, err := json.Marshal(p)
	if err != nil {
		return SendResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v3/smtp/email", bytes.NewReader(body))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.APIKey)
	resp, err := netutil.HTTPClient().Do(req)
	if err != nil {
		return SendResult{}, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("brevo returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return SendResult{}, &transport.PermanentError{Err: err}
		}
		return SendResult{}, err
	}
	var result response
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return SendResult{}, err
	}
	if result.MessageID == "" && len(result.MessageIDs) > 0 {
		result.MessageID = result.MessageIDs[0]
	}
	return SendResult{ProviderMessageID: result.MessageID}, nil
}

func recipients(in []string) []recipient {
	out := make([]recipient, 0, len(in))
	for _, email := range in {
		out = append(out, recipient{Email: email})
	}
	return out
}
