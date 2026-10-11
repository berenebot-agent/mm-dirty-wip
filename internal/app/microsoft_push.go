package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

const MicrosoftPushPath = "/internal/ingest/microsoft/notifications"

func MicrosoftPushHash(state string) string {
	h := sha256.Sum256([]byte(state))
	return hex.EncodeToString(h[:])
}

func (m *RemoteMailboxService) WakeMicrosoftPush() {
	select {
	case m.pushWake <- struct{}{}:
	default:
	}
}

// AcceptMicrosoftPush persists work before the HTTP callback acknowledges it.
func (m *RemoteMailboxService) AcceptMicrosoftPush(ctx context.Context, id, state, lifecycle string) (bool, error) {
	if id == "" || state == "" || len(state) > 128 {
		return false, nil
	}
	ok, err := m.Service.Store.QueueMicrosoftPush(ctx, id, MicrosoftPushHash(state), lifecycle)
	if ok && err == nil {
		m.WakeMicrosoftPush()
	}
	return ok, err
}

func microsoftMissing(err error) bool {
	var mb *model.MailboxError
	return errors.As(err, &mb) && mb.Kind == model.ErrKindNotFound
}

// ReconcileMicrosoftPush is process-owned, serialized, and safe to repeat after
// a restart. Notifications are hints; durable delta cursors remain authoritative.
func (m *RemoteMailboxService) ReconcileMicrosoftPush(ctx context.Context) error {
	m.microsoftPushMu.Lock()
	defer m.microsoftPushMu.Unlock()
	inboxes, err := m.Service.Store.ListDetectableRemoteInboxes(ctx)
	if err != nil {
		return err
	}
	for _, inbox := range inboxes {
		c, err := m.Service.Store.MicrosoftConnection(ctx, inbox.AccountID, inbox.ID)
		if errors.Is(err, store.ErrNotFound) || c.Shared {
			continue
		}
		if err != nil {
			return err
		}
		p, err := m.Service.Store.MicrosoftPush(ctx, c.AccountID, c.InboxID)
		if errors.Is(err, store.ErrNotFound) {
			b := make([]byte, 32)
			if _, err = rand.Read(b); err != nil {
				return err
			}
			state := base64.RawURLEncoding.EncodeToString(b)
			enc, err := m.Service.EncryptSecretAAD("microsoft_push:"+c.AccountID+":"+c.InboxID, []byte(state))
			if err != nil {
				return err
			}
			p = store.MicrosoftPush{AccountID: c.AccountID, InboxID: c.InboxID, EncryptedState: enc, StateHash: MicrosoftPushHash(state), Status: "setting up"}
		} else if err != nil {
			return err
		}
		p.ClientID, p.Tenant, p.EncryptedToken = c.ClientID, c.Tenant, c.EncryptedToken
		if err = m.Service.Store.SaveMicrosoftPush(ctx, p); err != nil {
			return err
		}
	}
	ps, err := m.Service.Store.ListMicrosoftPush(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().Before(p.RetryAt) {
			continue
		}
		pass, cancel := context.WithTimeout(ctx, 2*time.Minute)
		err := m.processMicrosoftPush(pass, &p)
		cancel()
		if err != nil {
			// Generic, secret-free status; provider responses never enter UI/logs.
			p.Status = "retrying setup; polling fallback"
			p.RetryAt = time.Now().Add(time.Minute)
			if e := m.Service.Store.SaveMicrosoftPush(ctx, p); e != nil {
				return e
			}
		}
	}
	return nil
}

func (m *RemoteMailboxService) processMicrosoftPush(ctx context.Context, p *store.MicrosoftPush) error {
	c, err := m.Service.Store.MicrosoftConnection(ctx, p.AccountID, p.InboxID)
	box, boxErr := m.Service.Store.GetInboxInternal(ctx, p.AccountID, p.InboxID)
	removed := errors.Is(err, store.ErrNotFound) || errors.Is(boxErr, store.ErrNotFound) || (err == nil && boxErr == nil && (c.Shared || !box.RemoteConfigured || !box.Enabled))
	if removed {
		return m.removeMicrosoftPush(ctx, p)
	}
	if err != nil {
		return err
	}
	if boxErr != nil {
		return boxErr
	}
	callback := strings.TrimRight(m.Service.Config.ReceiverURL(), "/") + MicrosoftPushPath
	u, err := url.Parse(callback)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		p.Status = "polling; public HTTPS receiver required"
		p.RetryAt = time.Now().Add(time.Minute)
		return m.Service.Store.SaveMicrosoftPush(ctx, *p)
	}
	token, c, err := m.microsoftAccess(ctx, p.AccountID, p.InboxID)
	if err != nil {
		return err
	}
	p.EncryptedToken = c.EncryptedToken
	if p.SubscriptionID != "" && (p.CallbackURL != callback || p.Lifecycle == "subscriptionRemoved" || !p.ExpiresAt.After(time.Now())) {
		if err = m.Microsoft.DeleteSubscription(ctx, token, p.SubscriptionID); err != nil && !microsoftMissing(err) {
			return err
		}
		p.SubscriptionID = ""
		if err = m.Service.Store.SaveMicrosoftPush(ctx, *p); err != nil {
			return err
		}
		if err = m.Service.Store.ClearMicrosoftPushLifecycle(ctx, p.AccountID, p.InboxID, p.Requested); err != nil {
			return err
		}
		p.Lifecycle = ""
	}
	if p.SubscriptionID == "" {
		// Establish cursors before subscribing. Historical backfill remains silent.
		status, err := m.ReconcileRemote(ctx, p.AccountID, p.InboxID)
		if err != nil {
			return err
		}
		if status.Status != store.RemoteIndexComplete {
			p.Status = "setting up; indexing baseline"
			return m.Service.Store.SaveMicrosoftPush(ctx, *p)
		}
		state, err := m.Service.DecryptSecretAAD("microsoft_push:"+p.AccountID+":"+p.InboxID, p.EncryptedState)
		if err != nil {
			return err
		}
		resource := "users/" + url.PathEscape(c.IdentityID) + "/mailFolders('inbox')/messages"
		p.CallbackURL = callback
		p.ExpiresAt = time.Now().UTC().Add(6 * 24 * time.Hour)
		// Persist the create intent first. A lost response can be recovered by
		// listing and matching this secret, resource, and callback URL.
		if err = m.Service.Store.SaveMicrosoftPush(ctx, *p); err != nil {
			return err
		}
		subs, err := m.Microsoft.Subscriptions(ctx, token)
		if err != nil {
			return err
		}
		var sub microsoft.Subscription
		for _, s := range subs {
			if s.ClientState == string(state) && s.Resource == resource && s.NotificationURL == callback && s.ExpiresAt.After(time.Now()) {
				sub = s
				break
			}
		}
		if sub.ID == "" {
			sub, err = m.Microsoft.CreateSubscription(ctx, token, microsoft.Subscription{Resource: resource, ChangeType: "created,updated,deleted", NotificationURL: callback, LifecycleURL: callback, ClientState: string(state), ExpiresAt: time.Now().UTC().Add(6 * 24 * time.Hour)})
			if err != nil {
				return err
			}
		}
		if sub.ID == "" || !sub.ExpiresAt.After(time.Now()) {
			return errors.New("incomplete Microsoft subscription")
		}
		p.SubscriptionID, p.ExpiresAt = sub.ID, sub.ExpiresAt
		// Catch arrivals between the baseline and subscription establishment,
		// including notifications sent before the create response was persisted.
		if err = m.Service.Store.SaveMicrosoftPush(ctx, *p); err != nil {
			return err
		}
		if _, err = m.Service.Store.QueueMicrosoftPush(ctx, p.SubscriptionID, p.StateHash, ""); err != nil {
			return err
		}
		fresh, err := m.Service.Store.MicrosoftPush(ctx, p.AccountID, p.InboxID)
		if err != nil {
			return err
		}
		p.Requested, p.Completed, p.Lifecycle = fresh.Requested, fresh.Completed, fresh.Lifecycle
	} else if time.Until(p.ExpiresAt) < 24*time.Hour || p.Lifecycle == "reauthorizationRequired" {
		sub, err := m.Microsoft.RenewSubscription(ctx, token, p.SubscriptionID, time.Now().UTC().Add(6*24*time.Hour))
		if microsoftMissing(err) {
			p.SubscriptionID = ""
			p.Status = "setting up"
			return m.Service.Store.SaveMicrosoftPush(ctx, *p)
		}
		if err != nil {
			return err
		}
		if !sub.ExpiresAt.After(time.Now()) {
			return errors.New("incomplete Microsoft renewal")
		}
		p.ExpiresAt = sub.ExpiresAt
	}
	p.Status, p.RetryAt = "push active", time.Time{}
	if err = m.Service.Store.ClearMicrosoftPushLifecycle(ctx, p.AccountID, p.InboxID, p.Requested); err != nil {
		return err
	}
	if err = m.Service.Store.SaveMicrosoftPush(ctx, *p); err != nil {
		return err
	}
	if p.Requested > p.Completed {
		status, err := m.ReconcileRemote(ctx, p.AccountID, p.InboxID)
		if err != nil {
			return err
		}
		if status.Status != store.RemoteIndexComplete {
			m.WakeMicrosoftPush()
			return nil
		}
		return m.Service.Store.CompleteMicrosoftPush(ctx, p.AccountID, p.InboxID, p.Requested)
	}
	return nil
}

func (m *RemoteMailboxService) removeMicrosoftPush(ctx context.Context, p *store.MicrosoftPush) error {
	if p.CallbackURL != "" && p.ExpiresAt.After(time.Now()) {
		b, err := m.Service.DecryptSecretAAD(MicrosoftAAD(p.AccountID, p.InboxID), p.EncryptedToken)
		if err != nil {
			return err
		}
		var grant microsoft.Token
		if err = json.Unmarshal(b, &grant); err != nil {
			return err
		}
		token, err := m.Microsoft.Refresh(ctx, p.Tenant, p.ClientID, grant.RefreshToken)
		if err != nil {
			return err
		}
		if token.RefreshToken == "" {
			token.RefreshToken = grant.RefreshToken
		}
		b, _ = json.Marshal(token)
		p.EncryptedToken, err = m.Service.EncryptSecretAAD(MicrosoftAAD(p.AccountID, p.InboxID), b)
		if err != nil {
			return err
		}
		if err = m.Service.Store.SaveMicrosoftPush(ctx, *p); err != nil {
			return err
		}
		ids := []string{p.SubscriptionID}
		if p.SubscriptionID == "" {
			state, err := m.Service.DecryptSecretAAD("microsoft_push:"+p.AccountID+":"+p.InboxID, p.EncryptedState)
			if err != nil {
				return err
			}
			subs, err := m.Microsoft.Subscriptions(ctx, token.AccessToken)
			if err != nil {
				return err
			}
			ids = nil
			for _, sub := range subs {
				if sub.ClientState == string(state) && sub.NotificationURL == p.CallbackURL {
					ids = append(ids, sub.ID)
				}
			}
		}
		for _, id := range ids {
			if err = m.Microsoft.DeleteSubscription(ctx, token.AccessToken, id); err != nil && !microsoftMissing(err) {
				return err
			}
		}
	}
	return m.Service.Store.DeleteMicrosoftPush(ctx, p.AccountID, p.InboxID)
}

func (w *RemoteWorker) runMicrosoftPush(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = w.remote.ReconcileMicrosoftPush(ctx)
		// Arrival delivery is independent of the worker's periodic poll tick.
		w.drainRemoteArrivals()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.remote.pushWake:
		}
	}
}
