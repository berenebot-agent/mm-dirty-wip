package httpapp

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

func (s *Server) microsoftPush(w http.ResponseWriter, r *http.Request) {
	logger := s.Log
	if logger == nil {
		logger = slog.Default()
	}
	if values, ok := r.URL.Query()["validationToken"]; ok {
		if len(values) != 1 || values[0] == "" || len(values[0]) > 4096 {
			http.Error(w, "invalid validation challenge", http.StatusBadRequest)
			return
		}
		logger.DebugContext(r.Context(), "microsoft push validation challenge")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, values[0])
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Value []struct {
			SubscriptionID string `json:"subscriptionId"`
			ClientState    string `json:"clientState"`
			Lifecycle      string `json:"lifecycleEvent"`
			ChangeType     string `json:"changeType"`
		} `json:"value"`
	}
	decoder := json.NewDecoder(r.Body)
	if decoder.Decode(&body) != nil || len(body.Value) == 0 || len(body.Value) > 1000 || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid notification batch", http.StatusBadRequest)
		return
	}
	for _, n := range body.Value {
		switch n.Lifecycle {
		case "reauthorizationRequired", "subscriptionRemoved", "missed":
		case "":
			if n.ChangeType != "created" && n.ChangeType != "updated" && n.ChangeType != "deleted" {
				continue
			}
		default:
			continue
		}
		// An accepted notification is a fetch hint, not an arrival: it only
		// persists durable work and wakes the push worker. It is therefore not
		// logged at INFO. A push-triggered delivery is reported by the worker's
		// "remote sync" line (trigger=push) and the arrival's "remote received"
		// line (source="microsoft push"), so per-callback logging would be
		// redundant. The matched inbox address is discarded for the same reason.
		_, accepted, err := s.remoteMailbox().AcceptMicrosoftPush(r.Context(), n.SubscriptionID, n.ClientState, n.Lifecycle)
		if err != nil {
			http.Error(w, "notification persistence unavailable", http.StatusServiceUnavailable)
			return
		}
		if !accepted {
			logger.DebugContext(r.Context(), "microsoft push ignored")
			continue
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
