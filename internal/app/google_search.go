package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// googleViewOperator maps a Gmail system label id to the search operator that
// selects it. Gmail's draft label id is DRAFT but its operator is "drafts".
func googleViewOperator(label string) string {
	if label == "DRAFT" {
		return "drafts"
	}
	return strings.ToLower(label)
}

// googleHigherViews returns the system labels that take precedence over a
// folder view. A message belongs to the first system label it carries, so a
// view must exclude the ones above it.
func googleHigherViews(path string) []string {
	for i, label := range store.GoogleFolderPrecedence {
		if label == path {
			return store.GoogleFolderPrecedence[:i]
		}
	}
	return nil
}

// googleSystemFolder reports whether a path names one of Gmail's system folder
// views.
func googleSystemFolder(path string) bool {
	for _, label := range store.GoogleFolderPrecedence {
		if label == path {
			return true
		}
	}
	return false
}

func (m *RemoteMailboxService) searchGoogle(ctx context.Context, p model.Principal, inbox string, q RemoteSearchQuery) (RemoteSearchResult, error) {
	if _, e := m.authorizeRead(ctx, p, inbox); e != nil {
		return RemoteSearchResult{}, e
	}
	client, t, e := m.GoogleMailboxClient(ctx, p.AccountID, inbox)
	if e != nil {
		return RemoteSearchResult{}, e
	}
	query := q.Text
	for k, v := range map[string]string{"from": q.From, "to": q.To, "subject": q.Subject, "label": q.Label} {
		if v != "" {
			query += " " + k + ":" + strconv.Quote(v)
		}
	}
	scope := m.googleFolderScope(ctx, p.AccountID, inbox, q.FolderPath)
	if scope != "" && !googleSystemFolder(scope) {
		// A user label is an additive label, not a folder: it selects no folder
		// view, so the search has no rows rather than the provider's own label
		// search.
		return RemoteSearchResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessComplete}, nil
	}
	label := scope
	if scope != "" {
		// A message belongs to the first system label it carries, so a view
		// excludes the labels that take precedence over it.
		for _, higher := range googleHigherViews(scope) {
			query += " -in:" + googleViewOperator(higher)
		}
	}
	if q.Unread != nil && *q.Unread {
		query += " is:unread"
	}
	if q.Flagged != nil && *q.Flagged {
		query += " is:starred"
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
	pageSize := 100
	if m.Service.Store.IsOne(ctx, p.AccountID, inbox) {
		pageSize = 10
	}
	page, e := client.List(ctx, t, strings.TrimSpace(query), label, cursor.Page, pageSize)
	if e != nil {
		return RemoteSearchResult{}, e
	}
	out := RemoteSearchResult{Items: []RemoteMessageView{}, Completeness: model.CompletenessComplete}
	if cursor.Offset > len(page.Messages) {
		return RemoteSearchResult{}, model.NewMailboxError(model.ErrKindInvalid, "Invalid Google search cursor", false, nil)
	}
	for i := cursor.Offset; i < len(page.Messages); i++ {
		g := page.Messages[i]
		g, e := client.Get(ctx, t, g.ID, "metadata")
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
