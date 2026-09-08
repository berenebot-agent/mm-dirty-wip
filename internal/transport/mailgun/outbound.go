package mailgun

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
}
type SendResult struct {
	ProviderMessageID string `json:"provider_message_id"`
}

func Send(ctx context.Context, c Config, m SendRequest) (SendResult, error) {
	base := strings.TrimRight(c.APIBase, "/")
	if base == "" {
		base = "https://api.mailgun.net"
	}
	if c.APIKey == "" || c.Domain == "" {
		return SendResult{}, fmt.Errorf("mailgun api_key and domain are required")
	}
	v := url.Values{}
	v.Set("from", m.From)
	for _, x := range m.To {
		v.Add("to", x)
	}
	for _, x := range m.CC {
		v.Add("cc", x)
	}
	for _, x := range m.BCC {
		v.Add("bcc", x)
	}
	v.Set("subject", m.Subject)
	v.Set("text", m.Text)
	if m.HTML != "" {
		v.Set("html", m.HTML)
	}
	if m.MessageID != "" {
		v.Set("h:Message-Id", m.MessageID)
	}
	if m.InReplyTo != "" {
		v.Set("h:In-Reply-To", m.InReplyTo)
	}
	if len(m.References) > 0 {
		v.Set("h:References", strings.Join(m.References, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v3/"+url.PathEscape(c.Domain)+"/messages", strings.NewReader(v.Encode()))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("api", c.APIKey)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return SendResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SendResult{}, fmt.Errorf("mailgun returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var payload struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &payload)
	return SendResult{ProviderMessageID: payload.ID}, nil
}
