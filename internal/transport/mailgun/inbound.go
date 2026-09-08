package mailgun

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gatehouse-mail/internal/transport"
)

type InboundForm struct {
	Timestamp         string
	Token             string
	Signature         string
	Sender            string
	Recipient         string
	ProviderMessageID string
	RawPath           string
	Size              int64
}

// Transport adapts Mailgun HTTPS webhooks to the generic inbound boundary.
type Transport struct{}

func init() { transport.RegisterInbound(Transport{}) }

func (Transport) Name() string { return "mailgun" }

func (Transport) Parse(r *http.Request, tmpPath string, maxBytes int64) (transport.InboundMessage, error) {
	form, err := ParseInboundRequest(r, tmpPath, maxBytes)
	if err != nil {
		return transport.InboundMessage{}, err
	}
	return transport.InboundMessage{
		Provider:          "mailgun",
		Recipient:         form.Recipient,
		EnvelopeFrom:      form.Sender,
		RawPath:           form.RawPath,
		Size:              form.Size,
		DeliveryID:        form.Token,
		ProviderMessageID: form.ProviderMessageID,
		Timestamp:         form.Timestamp,
		Token:             form.Token,
		Signature:         form.Signature,
	}, nil
}

func (Transport) Verify(_ *http.Request, msg transport.InboundMessage, secret string) error {
	if !VerifySignature(secret, msg.Timestamp, msg.Token, msg.Signature) {
		return transport.ErrInboundUnauthorized
	}
	return nil
}

func VerifySignature(signingKey, timestamp, token, signature string) bool {
	if signingKey == "" || timestamp == "" || token == "" || signature == "" {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(timestamp), 10, 64)
	if err != nil {
		return false
	}
	delta := time.Since(time.Unix(ts, 0))
	if delta < -15*time.Minute || delta > 15*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(signingKey))
	_, _ = mac.Write([]byte(timestamp + token))
	want := mac.Sum(nil)
	got, err := hex.DecodeString(strings.TrimSpace(signature))
	return err == nil && hmac.Equal(want, got)
}

func ParseInboundRequest(r *http.Request, tmpPath string, maxBytes int64) (InboundForm, error) {
	var out InboundForm
	ct := r.Header.Get("Content-Type")
	media, params, _ := mime.ParseMediaType(ct)
	if media == "multipart/form-data" {
		mr := multipartReader(r, params["boundary"])
		if mr == nil {
			return out, fmt.Errorf("invalid multipart boundary")
		}
		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return out, err
		}
		defer f.Close()
		var wrote bool
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return out, err
			}
			name := p.FormName()
			if name == "body-mime" {
				n, err := io.Copy(f, io.LimitReader(p, maxBytes+1))
				p.Close()
				if err != nil {
					return out, err
				}
				if n > maxBytes {
					return out, fmt.Errorf("message too large")
				}
				out.Size = n
				wrote = true
				continue
			}
			b, err := io.ReadAll(io.LimitReader(p, 1<<20))
			p.Close()
			if err != nil {
				return out, err
			}
			setField(&out, name, string(b))
		}
		if !wrote {
			return out, fmt.Errorf("body-mime missing")
		}
		out.RawPath = tmpPath
		return out, nil
	}
	if media == "application/x-www-form-urlencoded" || media == "" {
		r.Body = http.MaxBytesReader(nil, r.Body, maxBytes*2)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return out, err
		}
		vals, err := url.ParseQuery(string(b))
		if err != nil {
			return out, err
		}
		for k, v := range vals {
			if len(v) > 0 {
				setField(&out, k, v[0])
			}
		}
		raw := vals.Get("body-mime")
		if int64(len(raw)) > maxBytes {
			return out, fmt.Errorf("message too large")
		}
		if err = os.WriteFile(tmpPath, []byte(raw), 0o600); err != nil {
			return out, err
		}
		out.RawPath = tmpPath
		out.Size = int64(len(raw))
		return out, nil
	}
	return out, fmt.Errorf("unsupported content type %s", media)
}

// local wrapper avoids exposing multipart type in package API.
func multipartReader(r *http.Request, boundary string) *multipart.Reader {
	if boundary == "" {
		return nil
	}
	return multipart.NewReader(r.Body, boundary)
}
func setField(out *InboundForm, name, value string) {
	value = strings.TrimSpace(value)
	switch name {
	case "timestamp":
		out.Timestamp = value
	case "token":
		out.Token = value
	case "signature":
		out.Signature = value
	case "sender":
		out.Sender = value
	case "recipient":
		out.Recipient = value
	case "Message-Id", "message-id", "message_id":
		out.ProviderMessageID = value
	}
}
