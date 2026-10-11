package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

func (m *RemoteMailboxService) searchGoogle(ctx context.Context, p model.Principal, inbox string, q RemoteSearchQuery) (RemoteSearchResult, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteSearchResult{}, e
	}
	t, e := m.GoogleAccess(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteSearchResult{}, e
	}
	query := q.Text
	for k, v := range map[string]string{"from": q.From, "to": q.To, "subject": q.Subject, "label": q.Label} {
		if v != "" {
			query += " " + k + ":" + strconv.Quote(v)
		}
	}
	if q.FolderPath == "ARCHIVE" {
		query += " -in:inbox -in:trash -in:spam"
	}
	if q.Unread != nil && *q.Unread {
		query += " is:unread"
	}
	if q.Flagged != nil && *q.Flagged {
		query += " is:starred"
	}
	label := q.FolderPath
	if label == "ARCHIVE" {
		label = ""
	}
	var cursor struct {
		Page   string
		Offset int
	}
	if q.ProviderCursor != "" {
		b, e := base64.RawURLEncoding.DecodeString(q.ProviderCursor)
		if e != nil || json.Unmarshal(b, &cursor) != nil || cursor.Offset < 0 {
			return RemoteSearchResult{}, model.NewMailboxError(model.ErrKindInvalid, "Invalid Google search cursor", false, nil)
		}
	}
	encode := func(page string, offset int) string {
		b, _ := json.Marshal(struct {
			Page   string
			Offset int
		}{page, offset})
		return base64.RawURLEncoding.EncodeToString(b)
	}
	page, e := m.Google.List(ctx, t, strings.TrimSpace(query), label, cursor.Page, 100)
	if e != nil {
		return RemoteSearchResult{}, e
	}
	out := RemoteSearchResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessComplete}
	if cursor.Offset > len(page.Messages) {
		return RemoteSearchResult{}, model.NewMailboxError(model.ErrKindInvalid, "Invalid Google search cursor", false, nil)
	}
	for i := cursor.Offset; i < len(page.Messages); i++ {
		g := page.Messages[i]
		g, e := m.Google.Get(ctx, t, g.ID, "metadata")
		if e != nil {
			return RemoteSearchResult{}, e
		}
		v, e := m.cacheGoogle(ctx, p.AccountID, inbox, g)
		if e != nil {
			return RemoteSearchResult{}, e
		}
		next := ""
		if i+1 < len(page.Messages) {
			next = encode(cursor.Page, i+1)
		} else if page.NextPageToken != "" {
			next = encode(page.NextPageToken, 0)
		}
		v.ProviderCursor = next
		out.ProviderCursor = next
		out.Items = append(out.Items, v)
		if q.Limit > 0 && len(out.Items) >= q.Limit {
			break
		}
	}
	if len(out.Items) == 0 && page.NextPageToken != "" {
		out.ProviderCursor = encode(page.NextPageToken, 0)
	}
	return out, nil
}
