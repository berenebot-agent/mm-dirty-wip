package gmail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// errMessageTooLarge is a sentinel returned while streaming a message body that
// exceeds maxMessageBytes.
var errMessageTooLarge = errors.New("gmail: message exceeds the maximum size")

// invalid builds a safe invalid-argument *model.MailboxError.
func invalid(message string) error {
	return model.NewMailboxError(model.ErrKindInvalid, message, false, nil)
}

// networkError normalizes a transport-level failure for a non-send call. A
// client timeout and a connection failure are retryable.
func networkError(op string, err error) error {
	if netutil.IsClientTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return model.NewMailboxError(model.ErrKindRetryable, op+": request timed out", true, err)
	}
	if errors.Is(err, context.Canceled) {
		return model.NewMailboxError(model.ErrKindRetryable, op+": request canceled", false, err)
	}
	return model.NewMailboxError(model.ErrKindUnavailable, op+": cannot reach the Gmail API", true, err)
}

// ambiguousSend marks a send or draft whose outcome is unknown because the
// request may have been accepted without a readable response. The outbox must
// not retry it automatically.
func ambiguousSend(op string, err error) error {
	return &transport.AmbiguousError{
		Err: model.NewMailboxError(model.ErrKindRetryable, op+": the request outcome is unknown", true, err),
	}
}

// bodyError normalizes a failure to read or bound the request body. It is a
// definitive local failure, never ambiguous.
func bodyError(op string, err error) error {
	if errors.Is(err, errMessageTooLarge) {
		return model.NewMailboxError(model.ErrKindInvalid, op+": the message exceeds the maximum size", false, nil)
	}
	return model.NewMailboxError(model.ErrKindInternal, op+": could not read the message body", false, err)
}

// errorForResponse maps a non-2xx response onto a safe *model.MailboxError. The
// response body is read only to extract Google's stable error code; its raw text
// is never retained, so a response cannot leak a URL, credential or message
// body.
func errorForResponse(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	gstatus, reason, oauthCode := parseAPIError(body)
	return classify(op, resp.StatusCode, gstatus, reason, oauthCode)
}

// parseAPIError extracts Google's stable error signals from either the OAuth
// token-endpoint shape (error is a string) or the Gmail API shape (error is an
// object with status/reason). Arbitrary message text is deliberately discarded.
func parseAPIError(body []byte) (gstatus, reason, oauthCode string) {
	var outer struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
	}
	if err := json.Unmarshal(body, &outer); err != nil || len(outer.Error) == 0 {
		return "", "", ""
	}
	var asString string
	if err := json.Unmarshal(outer.Error, &asString); err == nil {
		return "", "", asString
	}
	var inner struct {
		Status string `json:"status"`
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(outer.Error, &inner); err == nil {
		gstatus = inner.Status
		if len(inner.Errors) > 0 {
			reason = inner.Errors[0].Reason
		}
	}
	return gstatus, reason, oauthCode
}

// classify maps a Google HTTP status and stable error code onto a normalized
// *model.MailboxError. 401 and OAuth invalid grant/client map to auth; 429 and
// the Gmail rate-limit reasons are retryable; a history 404 is not_found; 5xx is
// a retryable unavailability.
func classify(op string, code int, gstatus, reason, oauthCode string) error {
	switch oauthCode {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return model.NewMailboxError(model.ErrKindAuth, op+": the credentials were rejected", false, nil)
	case "invalid_request":
		return model.NewMailboxError(model.ErrKindInvalid, op+": the request was rejected", false, nil)
	case "temporarily_unavailable", "server_error":
		return model.NewMailboxError(model.ErrKindUnavailable, op+": the authorization server is temporarily unavailable", true, nil)
	}
	upper := strings.ToUpper(gstatus)
	if code == http.StatusUnauthorized || upper == "UNAUTHENTICATED" {
		return model.NewMailboxError(model.ErrKindAuth, op+": authentication failed or expired", false, nil)
	}
	if code == http.StatusTooManyRequests || upper == "RESOURCE_EXHAUSTED" ||
		reason == "rateLimitExceeded" || reason == "userRateLimitExceeded" {
		return model.NewMailboxError(model.ErrKindRetryable, op+": Gmail rate limit reached", true, nil)
	}
	switch code {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return model.NewMailboxError(model.ErrKindInvalid, op+": Gmail rejected the request", false, nil)
	case http.StatusForbidden:
		return model.NewMailboxError(model.ErrKindForbidden, op+": Gmail denied the request", false, nil)
	case http.StatusNotFound:
		return model.NewMailboxError(model.ErrKindNotFound, op+": Gmail could not find the resource", false, nil)
	case http.StatusConflict:
		return model.NewMailboxError(model.ErrKindConflict, op+": Gmail reported a conflict", false, nil)
	case http.StatusPreconditionFailed:
		return model.NewMailboxError(model.ErrKindConflict, op+": Gmail precondition failed", false, nil)
	}
	if code >= 500 {
		return model.NewMailboxError(model.ErrKindUnavailable, op+": Gmail is temporarily unavailable", true, nil)
	}
	return model.NewMailboxError(model.ErrKindInternal, op+": Gmail request failed", false, nil)
}
