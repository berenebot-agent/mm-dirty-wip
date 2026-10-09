package app

import (
	"context"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// ExternalAliasAdmin reports whether the principal may read or manage external
// sending aliases. They are an operator-managed capability; ordinary mailbox
// roles still authorize using an already configured identity. Reads and writes
// are gated identically.
func (s *Service) ExternalAliasAdmin(p model.Principal) error {
	if !p.Admin {
		return store.ErrForbidden
	}
	return nil
}

func (s *Service) externalAliasAdmin(p model.Principal) error {
	return s.ExternalAliasAdmin(p)
}

func (s *Service) CreateExternalAlias(ctx context.Context, p model.Principal, inboxID, address, display string) (store.ExternalAlias, error) {
	if err := s.externalAliasAdmin(p); err != nil {
		return store.ExternalAlias{}, err
	}
	return s.Store.CreateExternalAlias(ctx, p.AccountID, inboxID, address, display)
}

func (s *Service) UpdateExternalAlias(ctx context.Context, p model.Principal, inboxID, aliasID, display string) (store.ExternalAlias, error) {
	if err := s.externalAliasAdmin(p); err != nil {
		return store.ExternalAlias{}, err
	}
	return s.Store.UpdateExternalAlias(ctx, p.AccountID, inboxID, aliasID, display)
}

func (s *Service) DeleteExternalAlias(ctx context.Context, p model.Principal, inboxID, aliasID string) error {
	if err := s.externalAliasAdmin(p); err != nil {
		return err
	}
	return s.Store.DeleteExternalAlias(ctx, p.AccountID, inboxID, aliasID)
}

// SaveExternalAliasSendingConfig applies the same secret retention and provider
// validation as domain sending. Revision spans metadata, clear and config saves.
func (s *Service) SaveExternalAliasSendingConfig(ctx context.Context, p model.Principal, inboxID, aliasID, provider string, cfg map[string]any) (store.ExternalAlias, error) {
	if err := s.externalAliasAdmin(p); err != nil {
		return store.ExternalAlias{}, err
	}
	a, err := s.Store.GetExternalAlias(ctx, p.AccountID, inboxID, aliasID)
	if err != nil {
		return a, err
	}
	provider = normalizeProvider(provider)
	if provider == "" {
		return store.ExternalAlias{}, store.ErrInvalidAlias
	}
	if _, ok := transport.LookupOutbound(provider); !ok {
		return store.ExternalAlias{}, invalidConfig("unknown provider")
	}
	fields, err := outboundConfigFields(provider)
	if err != nil {
		return a, err
	}
	same := a.Configured && strings.EqualFold(a.Provider, provider)
	var old map[string]any
	if same {
		old, err = s.decryptConfig(configAAD(p.AccountID, "alias:"+aliasID), a.EncryptedConfig)
		if err != nil {
			return a, err
		}
	}
	merged, err := validateConfig(fields, cfg, old, same)
	if err != nil {
		return a, err
	}
	if err = s.validateProviderBase(merged); err != nil {
		return a, err
	}
	enc, err := s.encryptConfig(configAAD(p.AccountID, "alias:"+aliasID), merged)
	if err != nil {
		return a, err
	}
	saved, err := s.Store.SaveExternalAliasSendingConfig(ctx, p.AccountID, inboxID, aliasID, provider, enc, store.ConfigVersion{ID: a.ID, Revision: a.Revision})
	if err != nil {
		return saved, err
	}
	if _, err = s.Store.RequeuePendingForExternalAlias(ctx, p.AccountID, aliasID); err != nil {
		s.Log.Warn("requeue pending mail after alias connector save", "alias_id", aliasID, "error", err)
	}
	return saved, nil
}

func (s *Service) DeleteExternalAliasSendingConfig(ctx context.Context, p model.Principal, inboxID, aliasID string) error {
	if err := s.externalAliasAdmin(p); err != nil {
		return err
	}
	a, err := s.Store.GetExternalAlias(ctx, p.AccountID, inboxID, aliasID)
	if err != nil {
		return err
	}
	_, err = s.Store.SaveExternalAliasSendingConfig(ctx, p.AccountID, inboxID, aliasID, "", "", store.ConfigVersion{ID: a.ID, Revision: a.Revision})
	if err != nil {
		return err
	}
	_, err = s.Store.RequeuePendingForExternalAlias(ctx, p.AccountID, aliasID)
	if err != nil {
		s.Log.Warn("requeue pending mail after alias connector removal", "alias_id", aliasID, "error", err)
	}
	return nil
}

func (s *Service) DecryptExternalAliasSendingConfig(a store.ExternalAlias) (map[string]any, error) {
	return s.decryptConfig(configAAD(a.AccountID, "alias:"+a.ID), a.EncryptedConfig)
}
