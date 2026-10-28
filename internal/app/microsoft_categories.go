package app

import (
	"context"
	"errors"
	"strings"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
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

// RenameMicrosoftCategory renames a native Outlook category on Graph, so the
// messages carrying it carry the new name, then renames the local label that
// names it. Graph is the provider's truth and is touched first: if it refuses,
// nothing local changes.
func (m *RemoteMailboxService) RenameMicrosoftCategory(ctx context.Context, p model.Principal, inbox, oldName, newName string) error {
	if _, err := m.authorizeAssist(ctx, p, inbox); err != nil {
		return err
	}
	newName = strings.TrimSpace(newName)
	if clean, valid := model.NormalizeProviderLabel(newName); !valid || clean != newName {
		return model.NewMailboxError(model.ErrKindInvalid, "invalid label name", false, nil)
	}
	token, connection, err := m.microsoftAccess(ctx, p.AccountID, inbox)
	if err != nil {
		return err
	}
	category, err := m.Microsoft.RenameCategory(ctx, token, connection.Mailbox, oldName, newName)
	if err != nil {
		return err
	}
	return m.followCategory(ctx, p, inbox, oldName, category.DisplayName, "rename")
}

// DeleteMicrosoftCategory deletes a native Outlook category on Graph, which also
// clears it from every message carrying it, then removes the local label.
func (m *RemoteMailboxService) DeleteMicrosoftCategory(ctx context.Context, p model.Principal, inbox, name string) error {
	if _, err := m.authorizeAssist(ctx, p, inbox); err != nil {
		return err
	}
	token, connection, err := m.microsoftAccess(ctx, p.AccountID, inbox)
	if err != nil {
		return err
	}
	if err := m.Microsoft.DeleteCategory(ctx, token, connection.Mailbox, name); err != nil {
		return err
	}
	return m.followCategory(ctx, p, inbox, name, "", "delete")
}

// followCategory mirrors a Graph category rename or delete onto the local label
// catalogue. The provider change has already been applied, so a local failure is
// logged rather than reported as a failed rename or delete that did happen: the
// next provider sync re-derives the catalogue from Graph.
func (m *RemoteMailboxService) followCategory(ctx context.Context, p model.Principal, inbox, oldName, newName, operation string) error {
	if operation == "rename" {
		if _, err := m.Service.Store.CreateDefinedLabel(ctx, p, inbox, newName); err != nil {
			m.Service.Log.Warn("mirror renamed microsoft category", "inbox_id", inbox, "error", err)
		}
	}
	if _, err := m.Service.Store.ManageDefinedLabel(ctx, p, inbox, oldName, newName, operation); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.Service.Log.Warn("mirror microsoft category in the label catalogue", "inbox_id", inbox, "error", err)
	}
	return nil
}
