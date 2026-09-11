// Package resend adapts Resend webhooks to the generic inbound boundary and
// Resend's send API to the generic outbound boundary.
//
// Unlike Mailgun and Cloudflare, a Resend email.received webhook carries only
// metadata: the raw MIME must be fetched from the Resend API using the
// account's API key after the webhook signature is verified.
package resend

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gatehouse-mail/internal/transport"
	"gatehouse-mail/internal/transport/netutil"
)

// Transport adapts Resend inbound webhooks to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string        { return "resend" }
func (Transport) Description() string { return "Resend" }
func (Transport) IngestPath() string  { return "/internal/ingest/resend" }
func (Transport) ConfigFields() []transport.ConfigField {
	return []transport.ConfigField{
		{Name: "api_key", Label: "Resend key (full access)", Type: "password", Required: true, Secret: true, Placeholder: "re_..."},
		{Name: "webhook_secret", Label: "Webhook signing secret", Type: "password", Required: true, Secret: true, Placeholder: "whsec_..."},
		{Name: "api_base", Label: "Base URL", Type: "text", Placeholder: "https://api.resend.com"},
	}
}

const (
	defaultAPIBase  = "https://api.resend.com"
	eventReceived   = "email.received"
	maxWebhookBytes = 256 << 10
	// maxMetadataBytes bounds the retrieve-received-email metadata response.
	// Inline images are requested as cid references so the response stays
	// small; the cap only guards against an unexpectedly huge body.
	maxMetadataBytes = 32 << 20
	signatureMaxAge  = 5 * time.Minute
)

// Svix signature headers sent with every Resend webhook.
const (
	headerID        = "svix-id"
	headerTimestamp = "svix-timestamp"
	headerSignature = "svix-signature"
)

type webhook struct {
	Type string      `json:"type"`
	Data webhookData `json:"data"`
}

type webhookData struct {
	EmailID   string   `json:"email_id"`
	From      string   `json:"from"`
	To        []string `json:"to"`
	MessageID string   `json:"message_id"`
	Subject   string   `json:"subject"`
}

type receivedEmail struct {
	Raw *struct {
		DownloadURL string `json:"download_url"`
	} `json:"raw"`
}

// Receive authenticates the Svix signature, resolves the receive connection from
// the first recipient that maps to a configured domain, then fetches the raw
// MIME from the Resend API and stages it to tmpPath. Resend webhooks carry only
// metadata, so the MIME is pulled over an authenticated API call rather than
// read from the request body.
func (Transport) Receive(ctx context.Context, r *http.Request, resolver transport.BindingResolver, tmpPath string, maxBytes int64) (transport.InboundMessage, transport.InboundBinding, error) {
	var msg transport.InboundMessage
	var binding transport.InboundBinding

	r.Body = http.MaxBytesReader(nil, r.Body, maxWebhookBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return msg, binding, fmt.Errorf("read webhook: %w", err)
	}
	if len(body) == 0 {
		return msg, binding, fmt.Errorf("empty webhook")
	}
	var wh webhook
	if err = json.Unmarshal(body, &wh); err != nil {
		return msg, binding, fmt.Errorf("invalid webhook json: %w", err)
	}
	// Events other than email.received need no ingest. Acknowledge them so
	// Resend does not retry; no signature check or fetch is performed and no
	// state changes.
	if wh.Type != eventReceived {
		return msg, binding, transport.ErrInboundIgnored
	}
	binding, err = resolveBinding(ctx, resolver, wh.Data.To)
	if err != nil {
		return msg, binding, err
	}
	secret := configString(binding.Config, "webhook_secret")
	if !verifySignature(secret, r.Header, body) {
		return msg, binding, transport.ErrInboundUnauthorized
	}
	apiKey := configString(binding.Config, "api_key")
	if apiKey == "" {
		return msg, binding, fmt.Errorf("resend api_key is required")
	}
	rawURL, err := fetchRawURL(ctx, netutil.HTTPClient(), apiBase(binding.Config), apiKey, wh.Data.EmailID)
	if err != nil {
		return msg, binding, err
	}
	n, err := downloadToFile(ctx, netutil.HTTPClientLong(), rawURL, tmpPath, maxBytes)
	if err != nil {
		return msg, binding, err
	}
	return transport.InboundMessage{
		Provider:          "resend",
		Recipient:         binding.Recipient,
		EnvelopeFrom:      wh.Data.From,
		RawPath:           tmpPath,
		Size:              n,
		DeliveryID:        wh.Data.EmailID,
		ProviderMessageID: wh.Data.MessageID,
	}, binding, nil
}

// resolveBinding returns the receive connection for the first recipient that
// maps to a configured domain. Unknown recipients resolve to
// ErrInboundUnauthorized inside the service.
func resolveBinding(ctx context.Context, resolver transport.BindingResolver, recipients []string) (transport.InboundBinding, error) {
	for _, rcpt := range recipients {
		rcpt = strings.TrimSpace(rcpt)
		if rcpt == "" {
			continue
		}
		binding, err := resolver.ResolveInboundBinding(ctx, "resend", canonicalRecipient(rcpt))
		if err == nil {
			return binding, nil
		}
	}
	return transport.InboundBinding{}, transport.ErrInboundUnauthorized
}

// verifySignature validates the Svix HMAC over id.timestamp.body. The secret is
// the Resend webhook signing secret (whsec_ prefix, base64 payload).
func verifySignature(secret string, h http.Header, body []byte) bool {
	id := strings.TrimSpace(h.Get(headerID))
	ts := strings.TrimSpace(h.Get(headerTimestamp))
	if secret == "" || id == "" || ts == "" {
		return false
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	if delta := time.Since(time.Unix(unix, 0)); delta < -signatureMaxAge || delta > signatureMaxAge {
		return false
	}
	key, err := decodeSecret(secret)
	if err != nil || len(key) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, header := range h.Values(headerSignature) {
		for _, part := range strings.Fields(header) {
			version, encoded, ok := strings.Cut(part, ",")
			if !ok || version != "v1" {
				continue
			}
			got, err := base64.StdEncoding.DecodeString(encoded)
			if err == nil && hmac.Equal(want, got) {
				return true
			}
		}
	}
	return false
}

func decodeSecret(secret string) ([]byte, error) {
	secret = strings.TrimPrefix(strings.TrimSpace(secret), "whsec_")
	return base64.StdEncoding.DecodeString(secret)
}

// fetchRawURL retrieves the received email metadata and returns the signed raw
// MIME download URL.
func fetchRawURL(ctx context.Context, client *http.Client, base, apiKey, emailID string) (string, error) {
	if strings.TrimSpace(emailID) == "" {
		return "", fmt.Errorf("resend email_id missing")
	}
	if err := netutil.ValidateBaseURL(base); err != nil {
		return "", err
	}
	// html_format=cid keeps inline images as cid references instead of base64
	// data URIs, which otherwise make the response many megabytes and are not
	// used here: the raw MIME is fetched separately from the signed URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/emails/receiving/"+url.PathEscape(emailID)+"?html_format=cid", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return "", fmt.Errorf("resend retrieve returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var re receivedEmail
	if err = json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(&re); err != nil {
		return "", fmt.Errorf("resend retrieve decode: %w", err)
	}
	if re.Raw == nil || strings.TrimSpace(re.Raw.DownloadURL) == "" {
		return "", fmt.Errorf("resend raw content unavailable")
	}
	return re.Raw.DownloadURL, nil
}

// downloadToFile streams the signed raw MIME URL to tmpPath, bounded by
// maxBytes. The URL is issued by Resend over HTTPS; the scheme is enforced and
// the shared guarded client applies the public-routable destination check.
func downloadToFile(ctx context.Context, client *http.Client, rawURL, tmpPath string, maxBytes int64) (int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	if u.Scheme != "https" {
		return 0, fmt.Errorf("resend raw download must be https")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("resend raw download returned %s", resp.Status)
	}
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	closeErr := f.Close()
	if err != nil {
		return n, err
	}
	if closeErr != nil {
		return n, closeErr
	}
	if n > maxBytes {
		return n, fmt.Errorf("message too large")
	}
	if n == 0 {
		return n, fmt.Errorf("empty message")
	}
	return n, nil
}

// canonicalRecipient strips any display name and lowercases the address.
func canonicalRecipient(v string) string {
	if a, err := mail.ParseAddress(strings.TrimSpace(v)); err == nil {
		return strings.ToLower(a.Address)
	}
	return strings.ToLower(strings.TrimSpace(v))
}

func configString(cfg map[string]any, key string) string {
	v, _ := cfg[key].(string)
	return strings.TrimSpace(v)
}

func apiBase(cfg map[string]any) string {
	base := strings.TrimRight(configString(cfg, "api_base"), "/")
	if base == "" {
		base = defaultAPIBase
	}
	return base
}
