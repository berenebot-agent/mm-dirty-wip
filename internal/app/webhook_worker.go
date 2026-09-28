package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

type WebhookWorker struct {
	svc    *Service
	client *http.Client
}

func NewWebhookWorker(svc *Service) *WebhookWorker {
	return &WebhookWorker{svc: svc, client: netutil.HTTPClientLong()}
}

// SetHTTPClient replaces the outbound client (used by tests, which cannot dial
// a public destination).
func (w *WebhookWorker) SetHTTPClient(c *http.Client) { w.client = c }

func (w *WebhookWorker) RunOnce(ctx context.Context) error {
	d, err := w.svc.Store.NextWebhookDelivery(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	var body []byte
	if d.Client.Mode == "forward" {
		m, e := w.svc.Store.GetMessageByID(ctx, d.Client.AccountID, d.EntityID)
		if e != nil {
			return e
		}
		path, e := w.svc.dataPath(m.RawPath)
		if e != nil {
			return e
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		body, e = io.ReadAll(io.LimitReader(f, w.svc.Config.MaxMessageBytes+1))
		_ = f.Close()
		if e != nil {
			return e
		}
		if int64(len(body)) > w.svc.Config.MaxMessageBytes {
			return fmt.Errorf("raw message exceeds configured maximum")
		}
	} else {
		body, err = json.Marshal(map[string]any{"event": d.Type, "cursor": d.Cursor, "inbox_id": d.Client.InboxID, "message_id": d.EntityID})
		if err != nil {
			return err
		}
	}
	secret, err := w.svc.DecryptSecret(d.Client.SecretEncrypted)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Client.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if d.Client.Mode == "forward" {
		req.Header.Set("Content-Type", "message/rfc822")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-MailMoose-Event", d.Type)
	req.Header.Set("X-MailMoose-Delivery", d.Client.ID+":"+d.Cursor)
	req.Header.Set("X-MailMoose-Message-Id", d.EntityID)
	req.Header.Set("X-MailMoose-Cursor", d.Cursor)
	if d.Client.AuthMode == "bearer" {
		req.Header.Set("Authorization", "Bearer "+string(secret))
	} else {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(ts + "."))
		_, _ = mac.Write(body)
		req.Header.Set("X-MailMoose-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := w.client.Do(req)
	now := time.Now().UTC()
	deadline := now.Add(w.svc.Config.WebhookRetryWindow)
	if err == nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return w.svc.Store.RecordWebhookDelivery(ctx, d.Client.ID, d.EventID, true, "", now, deadline)
		}
		err = fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	var previous int
	_ = w.svc.Store.WebhookAttemptCount(ctx, d.Client.ID, d.EventID, &previous)
	delay := time.Second * time.Duration(1<<min(previous, 10))
	if delay > time.Hour {
		delay = time.Hour
	}
	return w.svc.Store.RecordWebhookDelivery(ctx, d.Client.ID, d.EventID, false, truncateWebhookError(err.Error()), now.Add(delay), deadline)
}

func truncateWebhookError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512]
	}
	return s
}
