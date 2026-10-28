package httpapp

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/dellarb/mammothmail/internal/store"
)

func (s *Server) uiDashboardPreferences(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := principal(r)
	if !p.Admin || p.UserID == "" || !p.ViaSession {
		writeError(w, http.StatusForbidden, "signed-in account administrator required")
		return
	}
	if r.Method == http.MethodGet {
		preferences, err := s.Service.Store.GetDashboardPreferences(r.Context(), p)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not load dashboard preferences")
			return
		}
		writeJSON(w, http.StatusOK, preferences)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var preferences store.DashboardPreferences
	if err := json.NewDecoder(r.Body).Decode(&preferences); err != nil {
		writeError(w, http.StatusBadRequest, "invalid dashboard preferences")
		return
	}
	validColumns := map[string]map[string]bool{
		"inboxes": {"name": true, "address": true, "unread": true, "pending": true, "size": true},
		"clients": {"name": true, "type": true},
		"domains": {"domain": true, "catchall": true, "sending": true, "receiving": true},
	}
	for table, spec := range preferences.Sort {
		if !validColumns[table][spec.Key] || (spec.Direction != "asc" && spec.Direction != "desc") {
			writeError(w, http.StatusBadRequest, "invalid dashboard sort")
			return
		}
	}
	for table, groups := range preferences.Groups {
		if table != "inboxes" && table != "clients" {
			writeError(w, http.StatusBadRequest, "invalid group table")
			return
		}
		for id, name := range groups {
			name = strings.TrimSpace(name)
			if len(id) > 128 || utf8.RuneCountInString(name) > 64 {
				writeError(w, http.StatusBadRequest, "invalid group name or item")
				return
			}
			groups[id] = name
		}
	}
	for table, groups := range preferences.Collapsed {
		if table != "inboxes" && table != "clients" {
			writeError(w, http.StatusBadRequest, "invalid group table")
			return
		}
		for name := range groups {
			if utf8.RuneCountInString(name) > 64 {
				writeError(w, http.StatusBadRequest, "invalid group name")
				return
			}
		}
	}
	for table := range preferences.ShowGroups {
		if table != "inboxes" && table != "clients" {
			writeError(w, http.StatusBadRequest, "invalid group table")
			return
		}
	}
	for table := range preferences.ShowConnectors {
		if table != "clients" {
			writeError(w, http.StatusBadRequest, "invalid connector table")
			return
		}
	}
	if err := s.Service.Store.SetDashboardPreferences(r.Context(), p, preferences); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save dashboard preferences")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
