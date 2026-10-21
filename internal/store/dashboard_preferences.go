package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/dellarb/mailmoose/internal/model"
)

// DashboardPreferences are personal display preferences, never mailbox policy.
type DashboardPreferences struct {
	Groups         map[string]map[string]string `json:"groups"`
	Collapsed      map[string]map[string]bool   `json:"collapsed"`
	ShowGroups     map[string]bool              `json:"showGroups"`
	ShowConnectors map[string]bool              `json:"showConnectors"`
	Sort           map[string]DashboardSort     `json:"sort"`
}

type DashboardSort struct {
	Key       string `json:"key"`
	Direction string `json:"direction"`
}

func (s *Store) GetDashboardPreferences(ctx context.Context, p model.Principal) (DashboardPreferences, error) {
	var preferences DashboardPreferences
	if !p.ViaSession || p.UserID == "" || !p.Admin {
		return preferences, ErrForbidden
	}
	var raw string
	err := s.read.QueryRowContext(ctx, `SELECT dashboard_preferences FROM users WHERE id=? AND account_id=?`, p.UserID, p.AccountID).Scan(&raw)
	if err != nil {
		return preferences, err
	}
	err = json.Unmarshal([]byte(raw), &preferences)
	return preferences, err
}

func (s *Store) SetDashboardPreferences(ctx context.Context, p model.Principal, preferences DashboardPreferences) error {
	if !p.ViaSession || p.UserID == "" || !p.Admin {
		return ErrForbidden
	}
	raw, err := json.Marshal(preferences)
	if err != nil {
		return err
	}
	if len(raw) > 256<<10 {
		return errors.New("dashboard preferences too large")
	}
	result, err := s.write.ExecContext(ctx, `UPDATE users SET dashboard_preferences=? WHERE id=? AND account_id=?`, string(raw), p.UserID, p.AccountID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrForbidden
	}
	return err
}
