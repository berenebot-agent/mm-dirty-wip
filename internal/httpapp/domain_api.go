package httpapp

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// domainConfigResponse is the redacted view of a domain's sending or receiving
// configuration. Secret fields are never included; the generated map is present
// only in the single response that minted new secrets.
type domainConfigResponse struct {
	DomainID   string            `json:"domain_id"`
	Configured bool              `json:"configured"`
	Provider   string            `json:"provider"`
	Config     map[string]any    `json:"config"`
	UpdatedAt  *time.Time        `json:"updated_at,omitempty"`
	WebhookURL string            `json:"webhook_url,omitempty"`
	Generated  map[string]string `json:"generated,omitempty"`
	KeyID      string            `json:"key_id,omitempty"`
	PublicKey  string            `json:"public_key,omitempty"`
	TXTRecord  string            `json:"txt_record,omitempty"`
}

// apiDomainSending serves the domain-scoped sending provider config:
//
//	GET    /v1/admin/domains/{id}/sending
//	PUT    /v1/admin/domains/{id}/sending
//	DELETE /v1/admin/domains/{id}/sending
func (s *Server) apiDomainSending(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.Service.Store.GetDomainSendingConfig(r.Context(), p.AccountID, id)
		if err != nil {
			if errors.Is(err, store.ErrNoProvider) {
				writeDomainConfigJSON(w, 200, domainConfigResponse{DomainID: id, Provider: "", Config: map[string]any{}})
				return
			}
			mapDomainConfigError(w, err)
			return
		}
		resp, err := s.domainSendingResponse(id, cfg)
		if err != nil {
			mapDomainConfigError(w, err)
			return
		}
		writeDomainConfigJSON(w, 200, resp)
	case http.MethodPut:
		var in struct {
			Provider string         `json:"provider"`
			Config   map[string]any `json:"config"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		cfg, err := s.Service.SaveDomainSendingConfig(r.Context(), p.AccountID, id, in.Provider, in.Config)
		if err != nil {
			mapDomainConfigError(w, err)
			return
		}
		resp, err := s.domainSendingResponse(id, cfg)
		if err != nil {
			mapDomainConfigError(w, err)
			return
		}
		writeDomainConfigJSON(w, 200, resp)
	case http.MethodDelete:
		if err := s.Service.Store.DeleteDomainSendingConfig(r.Context(), p.AccountID, id); err != nil {
			mapDomainConfigError(w, err)
			return
		}
		w.WriteHeader(204)
	}
}

// apiDomainReceiving serves the domain-scoped receiving provider config:
//
//	GET    /v1/admin/domains/{id}/receiving
//	PUT    /v1/admin/domains/{id}/receiving
//	DELETE /v1/admin/domains/{id}/receiving
func (s *Server) apiDomainReceiving(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.Service.Store.GetDomainReceivingConfig(r.Context(), p.AccountID, id)
		if err != nil {
			if errors.Is(err, store.ErrNoProvider) {
				if effective, resolveErr := s.Service.Store.ResolveDomainReceivingConfig(r.Context(), p.AccountID, id, "dialmx"); resolveErr == nil {
					resp, responseErr := s.domainReceivingResponse(r.Context(), id, effective)
					if responseErr != nil {
						mapDomainConfigError(w, responseErr)
						return
					}
					writeDomainConfigJSON(w, 200, resp)
					return
				}
				if _, domainErr := s.Service.Store.GetDomain(r.Context(), p.AccountID, id); domainErr != nil {
					mapDomainConfigError(w, domainErr)
					return
				}
				writeDomainConfigJSON(w, 200, domainConfigResponse{DomainID: id, Provider: "", Config: map[string]any{}})
				return
			}
			mapDomainConfigError(w, err)
			return
		}
		resp, err := s.domainReceivingResponse(r.Context(), id, cfg)
		if err != nil {
			mapDomainConfigError(w, err)
			return
		}
		writeDomainConfigJSON(w, 200, resp)
	case http.MethodPut:
		var in struct {
			Provider         string         `json:"provider"`
			Config           map[string]any `json:"config"`
			RegenerateSecret bool           `json:"regenerate_secret"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if in.Provider == "dialmx" && in.RegenerateSecret {
			if len(in.Config) != 0 {
				writeError(w, 400, "save configuration separately from key rotation")
				return
			}
			if _, err := s.Service.RotateDialMXCredential(r.Context(), p.AccountID, id); err != nil {
				mapDomainConfigError(w, err)
				return
			}
			cfg, err := s.Service.Store.ResolveDomainReceivingConfig(r.Context(), p.AccountID, id, "dialmx")
			if err != nil {
				mapDomainConfigError(w, err)
				return
			}
			resp, err := s.domainReceivingResponse(r.Context(), id, cfg)
			if err != nil {
				mapDomainConfigError(w, err)
				return
			}
			writeDomainConfigJSON(w, 200, resp)
			return
		}
		cfg, generated, err := s.Service.SaveDomainReceivingConfig(r.Context(), p.AccountID, id, in.Provider, in.Config, in.RegenerateSecret)
		if err != nil {
			mapDomainConfigError(w, err)
			return
		}
		resp, err := s.domainReceivingResponse(r.Context(), id, cfg)
		if err != nil {
			mapDomainConfigError(w, err)
			return
		}
		if len(generated) > 0 {
			resp.Generated = generated
		}
		writeDomainConfigJSON(w, 200, resp)
	case http.MethodDelete:
		if err := s.Service.Store.DeleteDomainReceivingConfig(r.Context(), p.AccountID, id); err != nil {
			mapDomainConfigError(w, err)
			return
		}
		w.WriteHeader(204)
	}
}

// apiDomainSendingDeliveries lists the delivery log for a domain's current
// sending activity, independent of which provider config currently exists:
//
//	GET /v1/admin/domains/{id}/sending/deliveries
func (s *Server) apiDomainSendingDeliveries(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	before := int64(0)
	if v := r.URL.Query().Get("before"); v != "" {
		before, _ = strconv.ParseInt(v, 10, 64)
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	attempts, err := s.Service.Store.ListDomainDeliveryAttempts(r.Context(), p.AccountID, r.PathValue("id"), limit, before)
	if err != nil {
		mapDomainConfigError(w, err)
		return
	}
	// Serialise an empty history as [] rather than null.
	if attempts == nil {
		attempts = []store.DeliveryAttempt{}
	}
	writeJSON(w, 200, attempts)
}

// apiDomainReceivingDeliveries lists the receiving side of a domain's two-way
// log: delivered inbound mail plus inbound mail blocked by the allowed-senders
// rule, newest first:
//
//	GET /v1/admin/domains/{id}/receiving/deliveries
func (s *Server) apiDomainReceivingDeliveries(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	if !adminOnly(w, p) {
		return
	}
	before := time.Time{}
	if v := strings.TrimSpace(r.URL.Query().Get("before")); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			before = t
		}
	}
	limit, ok := limitQuery(w, r)
	if !ok {
		return
	}
	entries, err := s.Service.Store.ListDomainReceivingLog(r.Context(), p.AccountID, r.PathValue("id"), limit, before)
	if err != nil {
		mapDomainConfigError(w, err)
		return
	}
	// Serialise an empty history as [] rather than null.
	if entries == nil {
		entries = []store.DomainLogEntry{}
	}
	writeJSON(w, 200, entries)
}

func (s *Server) domainSendingResponse(domainID string, cfg store.DomainSendingConfig) (domainConfigResponse, error) {
	dec, err := s.Service.DecryptDomainSendingConfig(cfg)
	if err != nil {
		return domainConfigResponse{}, err
	}
	updated := cfg.UpdatedAt
	return domainConfigResponse{
		DomainID:   domainID,
		Configured: true,
		Provider:   cfg.Provider,
		Config:     outboundNonsecretConfig(cfg.Provider, dec),
		UpdatedAt:  &updated,
	}, nil
}

func (s *Server) domainReceivingResponse(ctx context.Context, domainID string, cfg store.DomainReceivingConfig) (domainConfigResponse, error) {
	dec, err := s.Service.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		return domainConfigResponse{}, err
	}
	updated := cfg.UpdatedAt
	resp := domainConfigResponse{
		DomainID:   domainID,
		Configured: true,
		Provider:   cfg.Provider,
		Config:     inboundNonsecretConfig(cfg.Provider, dec),
		UpdatedAt:  &updated,
		WebhookURL: s.inboundWebhookURL(cfg.Provider),
	}
	if strings.EqualFold(cfg.Provider, "dialmx") {
		credential, err := s.Service.EnsureDialMXCredential(ctx, cfg.AccountID, domainID)
		if err != nil {
			return domainConfigResponse{}, err
		}
		resp.KeyID, resp.PublicKey = credential.KeyID, credential.PublicKey
		pub, _ := base64.RawURLEncoding.DecodeString(credential.PublicKey)
		resp.TXTRecord = mxwire.DomainTXT(credential.KeyID, pub)
	}
	return resp, nil
}

// outboundNonsecretConfig filters a decrypted outbound config down to the
// schema's non-secret fields for safe display.
func outboundNonsecretConfig(provider string, cfg map[string]any) map[string]any {
	out := map[string]any{}
	t, ok := transport.LookupOutbound(provider)
	if !ok {
		return out
	}
	sp, ok := t.(transport.ConfigSchemaProvider)
	if !ok {
		return out
	}
	for _, f := range sp.ConfigFields() {
		if f.Secret {
			continue
		}
		if v, ok := cfg[f.Name]; ok {
			out[f.Name] = v
		}
	}
	return out
}

// inboundNonsecretConfig filters a decrypted inbound config down to the
// schema's non-secret fields for safe display.
func inboundNonsecretConfig(provider string, cfg map[string]any) map[string]any {
	out := map[string]any{}
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return out
	}
	for _, f := range t.ConfigFields() {
		if f.Secret {
			continue
		}
		if v, ok := cfg[f.Name]; ok {
			out[f.Name] = v
		}
	}
	return out
}

// inboundWebhookURL is the public webhook URL an operator registers with an
// inbound provider, derived from the adapter's fixed ingest path.
func (s *Server) inboundWebhookURL(provider string) string {
	t, ok := transport.LookupInbound(provider)
	if !ok {
		return ""
	}
	paths, ok := t.(transport.IngestPathProvider)
	if !ok {
		return ""
	}
	return strings.TrimRight(s.Service.Config.ReceiverURL(), "/") + paths.IngestPath()
}

// writeDomainConfigJSON marks domain config responses as non-cacheable; the
// body may carry freshly generated credentials exactly once.
func writeDomainConfigJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, v)
}

// mapDomainConfigError maps config handler failures to HTTP. Validation faults
// carry a user-safe message; anything unrecognised is redacted as a 500 so
// genuine internal failures never leak details.
func mapDomainConfigError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not found")
	case errors.Is(err, store.ErrForbidden):
		writeError(w, 403, "forbidden")
	case errors.Is(err, store.ErrConflict):
		writeError(w, 409, "conflict")
	case errors.Is(err, store.ErrNoProvider):
		writeError(w, 404, "not found")
	case errors.Is(err, app.ErrInvalidConfig):
		writeError(w, 400, err.Error())
	case errors.Is(err, transport.ErrUnknownProvider):
		writeError(w, 400, "unknown provider")
	default:
		writeError(w, 500, "internal server error")
	}
}
