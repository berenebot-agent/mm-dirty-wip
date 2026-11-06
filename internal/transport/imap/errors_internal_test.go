// These tests live in package imap rather than under tests/unit because wrapErr
// is the package's error-classification seam and is unexported: the black-box
// convention cannot reach it, and driving a real connection drop through the
// fake server would test the socket rather than the classification. Every case
// here is an error value the IMAP library or the net stack actually returns; the
// behaviour under test is which MailboxError kind each one becomes.
package imap

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
)

func TestWrapErrClassifiesDroppedConnection(t *testing.T) {
	t.Parallel()
	// A provider that closes the connection mid-command returns EOF: the socket
	// was open and the read produced no bytes. It must read as an unavailable
	// remote connection (retryable), never as an internal error carrying the raw
	// Go text, because a location action decides "reconcile" versus "retry" from
	// this classification.
	cases := []struct {
		name string
		err  error
	}{
		{"unexpected eof", io.ErrUnexpectedEOF},
		{"eof", io.EOF},
		{"wrapped unexpected eof", fmt.Errorf("imap: uid move: %w", io.ErrUnexpectedEOF)},
		{"closed connection", net.ErrClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := wrapErr(tc.err)
			var mb *model.MailboxError
			if !errors.As(err, &mb) {
				t.Fatalf("wrapErr(%v) = %T, want *model.MailboxError", tc.err, err)
			}
			if mb.Kind != model.ErrKindUnavailable {
				t.Fatalf("kind = %q, want %q", mb.Kind, model.ErrKindUnavailable)
			}
			if !mb.Retryable {
				t.Fatal("a dropped connection must stay retryable")
			}
			if mb.Message != "remote connection is closed" {
				t.Fatalf("message = %q, want the provider-neutral connection text", mb.Message)
			}
			if errors.Is(err, tc.err) {
				return
			}
			t.Fatalf("the cause must stay wrapped: %v", err)
		})
	}
}

// TestWrapErrKeepsInternalClassification pins the other side: an error that is
// not a connection failure must not be promoted to retryable by the EOF branch.
func TestWrapErrKeepsInternalClassification(t *testing.T) {
	t.Parallel()
	err := wrapErr(errors.New("imap: cannot parse the message set"))
	var mb *model.MailboxError
	if !errors.As(err, &mb) {
		t.Fatalf("wrapErr = %T, want *model.MailboxError", err)
	}
	if mb.Kind != model.ErrKindInternal || mb.Retryable {
		t.Fatalf("kind = %q retryable = %v, want internal and not retryable", mb.Kind, mb.Retryable)
	}
	if mb.Message != "" {
		t.Fatalf("message = %q, want empty so the caller reads the cause", mb.Message)
	}
}
