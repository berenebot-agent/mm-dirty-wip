package app

import (
	"context"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// CreateMicrosoftCategory persists a definition only after Graph accepts it.
func (m *RemoteMailboxService) CreateMicrosoftCategory(ctx context.Context, p model.Principal, inbox, name string) (string, error) {
	if _, err := m.authorizeAssist(ctx, p, inbox); err != nil {
		return "", err
	}
	clean, valid := model.NormalizeProviderLabel(strings.TrimSpace(name))
	if !valid {
		return "", model.NewMailboxError(model.ErrKindInvalid, "invalid label name", false, nil)
	}
	token, connection, err := m.microsoftAccess(ctx, p.AccountID, inbox)
	if err != nil {
		return "", err
	}
	categories, err := m.Microsoft.Categories(ctx, token, connection.Mailbox)
	if err != nil {
		return "", err
	}
	for _, category := range categories {
		if strings.EqualFold(category.DisplayName, clean) {
			return m.Service.Store.CreateDefinedLabel(ctx, p, inbox, category.DisplayName)
		}
	}
	category, err := m.Microsoft.CreateCategory(ctx, token, connection.Mailbox, clean)
	if err != nil {
		return "", err
	}
	return m.Service.Store.CreateDefinedLabel(ctx, p, inbox, category.DisplayName)
}
