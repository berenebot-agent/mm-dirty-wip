package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/dellarb/mammothmail/internal/model"
	"net/url"
	"time"
)

type Subscription struct {
	ID              string    `json:"id,omitempty"`
	Resource        string    `json:"resource,omitempty"`
	ChangeType      string    `json:"changeType,omitempty"`
	NotificationURL string    `json:"notificationUrl,omitempty"`
	LifecycleURL    string    `json:"lifecycleNotificationUrl,omitempty"`
	ClientState     string    `json:"clientState,omitempty"`
	ExpiresAt       time.Time `json:"expirationDateTime"`
}

func (c *Client) CreateSubscription(ctx context.Context, token string, s Subscription) (Subscription, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return Subscription{}, err
	}
	var out Subscription
	err = c.request(ctx, token, "POST", "/subscriptions", "", bytes.NewReader(b), "application/json", &out, nil, false)
	return out, err
}

func (c *Client) RenewSubscription(ctx context.Context, token, id string, expiry time.Time) (Subscription, error) {
	b, _ := json.Marshal(map[string]any{"expirationDateTime": expiry})
	var out Subscription
	err := c.request(ctx, token, "PATCH", "/subscriptions/"+url.PathEscape(id), "", bytes.NewReader(b), "application/json", &out, nil, false)
	return out, err
}

func (c *Client) DeleteSubscription(ctx context.Context, token, id string) error {
	return c.request(ctx, token, "DELETE", "/subscriptions/"+url.PathEscape(id), "", nil, "", nil, nil, false)
}

// Listing reconciles an ambiguous create without creating orphan subscriptions.
func (c *Client) Subscriptions(ctx context.Context, token string) ([]Subscription, error) {
	var subscriptions []Subscription
	cursor := ""
	for page := 0; page < 100; page++ {
		var out struct {
			Value []Subscription `json:"value"`
			Next  string         `json:"@odata.nextLink"`
		}
		if err := c.request(ctx, token, "GET", "/subscriptions", cursor, nil, "", &out, nil, false); err != nil {
			return nil, err
		}
		subscriptions = append(subscriptions, out.Value...)
		if out.Next == "" {
			return subscriptions, nil
		}
		cursor = out.Next
	}
	return nil, failure(model.ErrKindUnavailable, "subscription enumeration exceeds limit", false)
}
