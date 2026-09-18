package httpapp

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func externalAliasView(a store.ExternalAlias) model.ExternalAlias { return a.ExternalAlias }

func externalAliasError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
	case errors.Is(err, app.ErrInvalidConfig), errors.Is(err, store.ErrInvalidAlias):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNoProvider):
		writeError(w, http.StatusNotFound, "sending connector not configured")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (s *Server) apiExternalAliases(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.ExternalAliasAdmin(p); err != nil {
		externalAliasError(w, err)
		return
	}
	id := r.PathValue("id")
	// Scope the collection to the requested inbox in this account, so a foreign
	// or missing inbox is 404 rather than an empty list.
	if _, err := s.Service.Store.GetInboxInternal(r.Context(), p.AccountID, id); err != nil {
		externalAliasError(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		groups, err := s.Service.Store.ListExternalAliases(r.Context(), p.AccountID)
		if err != nil {
			externalAliasError(w, err)
			return
		}
		out := groups[id]
		if out == nil {
			out = []model.ExternalAlias{}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var in struct {
			Address     string `json:"address"`
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		a, err := s.Service.CreateExternalAlias(r.Context(), p, id, in.Address, in.DisplayName)
		if err != nil {
			externalAliasError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, externalAliasView(a))
	}
}

func (s *Server) apiExternalAlias(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.ExternalAliasAdmin(p); err != nil {
		externalAliasError(w, err)
		return
	}
	inboxID, aliasID := r.PathValue("id"), r.PathValue("aliasID")
	switch r.Method {
	case http.MethodPatch:
		var in struct {
			DisplayName string `json:"display_name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		a, err := s.Service.UpdateExternalAlias(r.Context(), p, inboxID, aliasID, in.DisplayName)
		if err != nil {
			externalAliasError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, externalAliasView(a))
	case http.MethodDelete:
		if err := s.Service.DeleteExternalAlias(r.Context(), p, inboxID, aliasID); err != nil {
			externalAliasError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) apiExternalAliasSending(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.ExternalAliasAdmin(p); err != nil {
		externalAliasError(w, err)
		return
	}
	inboxID, aliasID := r.PathValue("id"), r.PathValue("aliasID")
	a, err := s.Service.Store.GetExternalAlias(r.Context(), p.AccountID, inboxID, aliasID)
	if err != nil {
		externalAliasError(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		resp := map[string]any{"alias_id": aliasID, "configured": a.Configured, "provider": a.Provider, "config": map[string]any{}}
		if a.Configured {
			cfg, derr := s.Service.DecryptExternalAliasSendingConfig(a)
			if derr != nil {
				externalAliasError(w, derr)
				return
			}
			resp["config"] = outboundNonsecretConfig(a.Provider, cfg)
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, resp)
	case http.MethodPut:
		var in struct {
			Provider string         `json:"provider"`
			Config   map[string]any `json:"config"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		saved, err := s.Service.SaveExternalAliasSendingConfig(r.Context(), p, inboxID, aliasID, in.Provider, in.Config)
		if err != nil {
			externalAliasError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, externalAliasView(saved))
	case http.MethodDelete:
		if err := s.Service.DeleteExternalAliasSendingConfig(r.Context(), p, inboxID, aliasID); err != nil {
			externalAliasError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) apiExternalAliasDeliveries(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if err := s.Service.ExternalAliasAdmin(p); err != nil {
		externalAliasError(w, err)
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	rows, err := s.Service.Store.ListExternalAliasDeliveryAttempts(r.Context(), p.AccountID, r.PathValue("id"), r.PathValue("aliasID"), limit, before)
	if err != nil {
		externalAliasError(w, err)
		return
	}
	if rows == nil {
		rows = []store.DeliveryAttempt{}
	}
	writeJSON(w, http.StatusOK, rows)
}
