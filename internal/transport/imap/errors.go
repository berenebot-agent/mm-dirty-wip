package imap

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/emersion/go-imap/v2"
)

// Errors the adapter returns in addition to *model.MailboxError. They are
// comparable with errors.Is so callers can branch without inspecting a
// provider type.
var (
	// ErrNotConnected is returned when an operation is attempted on an adapter
	// that no longer holds a live session.
	ErrNotConnected = errors.New("imap: not connected")
	// ErrUnsupported is returned when the server cannot perform an operation
	// (for example an UID-targeted expunge without UIDPLUS). The returned
	// error is always also a *model.MailboxError of kind unsupported.
	ErrUnsupported = errors.New("imap: operation unsupported by server")
	// ErrNotFound is returned when a targeted message does not exist in the
	// selected folder. The returned error is also a *model.MailboxError of kind
	// not_found.
	ErrNotFound = errors.New("imap: message not found")
	// ErrUIDValidityChanged is returned when the folder's UIDVALIDITY no longer
	// matches the locator the caller supplied: the stored UID is no longer
	// trustworthy and must be re-resolved.
	ErrUIDValidityChanged = errors.New("imap: uid validity changed")
	// ErrAmbiguous is returned when an append or move could not be resolved to
	// exactly one remote message: the operation may or may not have taken
	// effect and the caller must reconcile by marker/Message-ID rather than
	// assume exactly-once.
	ErrAmbiguous = errors.New("imap: ambiguous remote outcome")
)

// Unsupported builds an unsupported *model.MailboxError, wrapping ErrUnsupported
// so both errors.Is checks and the normalized kind agree.
func Unsupported(message string) error {
	if message == "" {
		message = "operation is not supported by the remote server"
	}
	return model.NewMailboxError(model.ErrKindUnsupported, message, false, fmt.Errorf("%w: %s", ErrUnsupported, message))
}

// NotFound builds a not_found *model.MailboxError.
func NotFound(message string) error {
	if message == "" {
		message = "message not found"
	}
	return model.NewMailboxError(model.ErrKindNotFound, message, false, fmt.Errorf("%w: %s", ErrNotFound, message))
}

// conflictError builds a conflict *model.MailboxError carrying a stable,
// provider-neutral code (for example uid_validity_changed) as its message.
func conflictError(code, message string) error {
	return model.NewMailboxError(model.ErrKindConflict, message, false, fmt.Errorf("%w: %s", ErrUIDValidityChanged, code))
}

// Ambiguous builds a retryable *model.MailboxError of kind retryable: the
// remote outcome is unknown and must be reconciled before retrying.
func Ambiguous(message string, cause error) error {
	if message == "" {
		message = "remote outcome is ambiguous"
	}
	return model.NewMailboxError(model.ErrKindRetryable, message, true, fmt.Errorf("%w: %v", ErrAmbiguous, cause))
}

// wrapErr maps a low-level error from the IMAP library or the network stack onto
// a normalized *model.MailboxError. It never includes credentials in the
// message: only stable, provider-neutral text is surfaced.
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		return err
	}
	// A network timeout is retryable.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return model.NewMailboxError(model.ErrKindRetryable, "remote server timed out", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return model.NewMailboxError(model.ErrKindRetryable, "remote server timed out", true, err)
	}
	if errors.Is(err, context.Canceled) {
		return model.NewMailboxError(model.ErrKindRetryable, "operation canceled", false, err)
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, ErrNotConnected) {
		return model.NewMailboxError(model.ErrKindUnavailable, "remote connection is closed", true, err)
	}

	var statusErr *imap.Error
	if errors.As(err, &statusErr) {
		return wrapIMAPError(statusErr)
	}

	// A connection-level failure at dial time surfaces as *net.OpError or a TLS
	// error; classify as unavailable/retryable rather than internal.
	if isConnectionError(err) {
		return model.NewMailboxError(model.ErrKindUnavailable, "cannot reach remote server", true, err)
	}
	return model.NewMailboxError(model.ErrKindInternal, "", false, err)
}

// wrapIMAPError classifies an IMAP NO/BAD status response.
func wrapIMAPError(e *imap.Error) error {
	text := strings.TrimSpace(e.Text)
	lower := strings.ToLower(text)
	switch e.Code {
	case imap.ResponseCodeAuthenticationFailed, imap.ResponseCodeAuthorizationFailed:
		return model.NewMailboxError(model.ErrKindAuth, "remote server rejected the credentials", false, e)
	case imap.ResponseCodeNonExistent:
		return model.NewMailboxError(model.ErrKindNotFound, "remote folder or message does not exist", false, e)
	case imap.ResponseCodeAlreadyExists:
		return model.NewMailboxError(model.ErrKindConflict, "remote folder already exists", false, e)
	case imap.ResponseCodeOverQuota, imap.ResponseCodeTooBig:
		return model.NewMailboxError(model.ErrKindQuota, "remote mailbox is over quota", false, e)
	case imap.ResponseCodeCannot, imap.ResponseCodeNoPerm:
		return model.NewMailboxError(model.ErrKindForbidden, "remote server refused the operation", false, e)
	case imap.ResponseCodeHasChildren:
		return model.NewMailboxError(model.ErrKindConflict, "remote folder still has children", false, e)
	case imap.ResponseCodeTryCreate:
		return model.NewMailboxError(model.ErrKindNotFound, "remote destination does not exist", false, e)
	}
	// Fall back to text heuristics for servers that send no response code.
	switch {
	case strings.Contains(lower, "authenticat") || strings.Contains(lower, "login failed") || strings.Contains(lower, "invalid credential"):
		return model.NewMailboxError(model.ErrKindAuth, "remote server rejected the credentials", false, e)
	case strings.Contains(lower, "no such"), strings.Contains(lower, "not found"), strings.Contains(lower, "unknown mailbox"):
		return model.NewMailboxError(model.ErrKindNotFound, "remote folder or message does not exist", false, e)
	case strings.Contains(lower, "quota"), strings.Contains(lower, "over limit"):
		return model.NewMailboxError(model.ErrKindQuota, "remote mailbox is over quota", false, e)
	case strings.Contains(lower, "permission") || strings.Contains(lower, "denied") || strings.Contains(lower, "read-only"):
		return model.NewMailboxError(model.ErrKindForbidden, "remote server refused the operation", false, e)
	}
	if e.Type == imap.StatusResponseTypeBad {
		return model.NewMailboxError(model.ErrKindInvalid, "remote server rejected the request", false, e)
	}
	return model.NewMailboxError(model.ErrKindUnavailable, "remote server refused the operation", true, e)
}

func isConnectionError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "certificate") ||
		strings.Contains(msg, "tls")
}
