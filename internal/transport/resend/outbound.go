package resend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// Config is the decrypted outbound credential for Resend.
type Config struct {
	APIKey  string `json:"api_key"`
	APIBase string `json:"api_base,omitempty"`
}

type attachment struct {
	Filename string `json:"filename"`
	Content  []byte `json:"content"`
}

type payload struct {
	From        string            `json:"from"`
	To          []string          `json:"to"`
	CC          []string          `json:"cc,omitempty"`
	BCC         []string          `json:"bcc,omitempty"`
	Subject     string            `json:"subject"`
	HTML        string            `json:"html,omitempty"`
	Text        string            `json:"text,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []attachment      `json:"attachments,omitempty"`
}

type response struct {
	ID string `json:"id"`
}

type outboundTransport struct{}

func init() { transport.RegisterOutbound(outboundTransport{}) }

func (outboundTransport) Name() string        { return "resend" }
func (outboundTransport) Description() string { return "Resend" }
func (outboundTransport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "api_key", Label: "Resend key", Type: "password", Required: true, Secret: true, Placeholder: "re_..."},
		{Name: "api_base", Label: "Base URL", Type: "text", Placeholder: "https://api.resend.com"},
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
		return SendResult{}, fmt.Errorf("resend api_key is required")
	}
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = defaultAPIBase
	}
	from := m.FromAddress
	if m.FromName != "" {
		from = (&mail.Address{Name: m.FromName, Address: m.FromAddress}).String()
	}
	p := payload{
		From:    from,
		To:      m.To,
		CC:      m.CC,
		BCC:     m.BCC,
		Subject: m.Subject,
		HTML:    m.HTML,
		Text:    m.Text,
	}
	if m.MessageID != "" || m.InReplyTo != "" || len(m.References) > 0 {
		p.Headers = map[string]string{}
		if m.MessageID != "" {
			p.Headers["Message-ID"] = m.MessageID
		}
		if m.InReplyTo != "" {
			p.Headers["In-Reply-To"] = m.InReplyTo
		}
		if len(m.References) > 0 {
			p.Headers["References"] = strings.Join(m.References, " ")
		}
	}
	for _, a := range m.Attachments {
		p.Attachments = append(p.Attachments, attachment{Filename: a.Filename, Content: a.Content})
	}
	body, err := json.Marshal(p)
	if err != nil {
		return SendResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/emails", bytes.NewReader(body))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	// Resend honours an Idempotency-Key header: retrying the same logical send
	// after a lost success response returns the original result instead of
	// delivering a duplicate.
	if m.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", m.IdempotencyKey)
	}
	if err := netutil.ValidateBaseURL(base); err != nil {
		return SendResult{}, err
	}
	resp, err := netutil.HTTPClient().Do(req)
	if err != nil {
		return SendResult{}, err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := transport.ProviderError("resend", resp.Status, responseBody)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return SendResult{}, &transport.PermanentError{Err: err}
		}
		return SendResult{}, err
	}
	var result response
	if err = json.Unmarshal(responseBody, &result); err != nil {
		return SendResult{}, err
	}
	return SendResult{ProviderMessageID: result.ID}, nil
}
