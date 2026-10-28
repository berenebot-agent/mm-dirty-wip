package microsoft

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/dellarb/mammothmail/internal/model"
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

// categoryID resolves a category's display name to the master category id Graph
// addresses it by. Names are matched case-insensitively, the way Outlook treats
// them; a name that no category carries is not found rather than guessed at.
func (c *Client) categoryID(ctx context.Context, token, mailbox, name string) (string, error) {
	categories, err := c.Categories(ctx, token, mailbox)
	if err != nil {
		return "", err
	}
	for _, category := range categories {
		if strings.EqualFold(category.DisplayName, name) {
			return category.ID, nil
		}
	}
	return "", failure(model.ErrKindNotFound, "no category with that name", false)
}

// RenameCategory renames a native Outlook master category in place. Messages
// carrying it keep carrying it: Graph rewrites the category name on them.
func (c *Client) RenameCategory(ctx context.Context, token, mailbox, name, newName string) (Category, error) {
	id, err := c.categoryID(ctx, token, mailbox, name)
	if err != nil {
		return Category{}, err
	}
	var out Category
	body, err := json.Marshal(map[string]string{"displayName": newName})
	if err != nil {
		return Category{}, err
	}
	err = c.request(ctx, token, "PATCH", mailboxPath(mailbox)+"/outlook/masterCategories/"+url.PathEscape(id), "", bytes.NewReader(body), "application/json", &out, nil, false)
	return out, err
}

// DeleteCategory deletes a native Outlook master category, which also removes it
// from every message carrying it.
func (c *Client) DeleteCategory(ctx context.Context, token, mailbox, name string) error {
	id, err := c.categoryID(ctx, token, mailbox, name)
	if err != nil {
		return err
	}
	return c.request(ctx, token, "DELETE", mailboxPath(mailbox)+"/outlook/masterCategories/"+url.PathEscape(id), "", nil, "", nil, nil, false)
}
