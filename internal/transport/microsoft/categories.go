package microsoft

import (
	"context"
	"encoding/json"
	"strings"
)

type Category struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Color       string `json:"color"`
}

func (c *Client) Categories(ctx context.Context, token, mailbox string) ([]Category, error) {
	path := mailboxPath(mailbox) + "/outlook/masterCategories"
	out := []Category{}
	cursor := ""
	for {
		var page struct {
			Items []Category `json:"value"`
			Next  string     `json:"@odata.nextLink"`
		}
		if err := c.request(ctx, token, "GET", path, cursor, nil, "", &page, nil, false); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Next == "" {
			return out, nil
		}
		if page.Next == cursor {
			return nil, failure("invalid", "category pagination did not advance", false)
		}
		cursor = page.Next
	}
}

// CreateCategory creates a native Outlook master category, not a mail folder.
func (c *Client) CreateCategory(ctx context.Context, token, mailbox, name string) (Category, error) {
	var out Category
	body, err := json.Marshal(map[string]string{"displayName": name, "color": "preset0"})
	if err != nil {
		return out, err
	}
	err = c.request(ctx, token, "POST", mailboxPath(mailbox)+"/outlook/masterCategories", "", strings.NewReader(string(body)), "application/json", &out, nil, false)
	return out, err
}
