package mailparse

import (
	"bufio"
	"bytes"
	"io"
	"net/mail"
	"net/textproto"
	"strings"

	"github.com/dellarb/mailmoose/internal/messagepreview"
)

const PreviewBytesLimit = 64 << 10

// PreviewBytes extracts a best-effort preview from a bounded MIME prefix. It
// tolerates a truncated final part, skips attachments, and never changes the
// strict parsing used by ingestion or approval classification.
func PreviewBytes(raw []byte) string {
	if len(raw) > PreviewBytesLimit {
		raw = raw[:PreviewBytesLimit]
	}
	msg, err := mail.ReadMessage(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		return ""
	}
	var plain, markup string
	_ = walkMIME(textproto.MIMEHeader(msg.Header), msg.Body, func(info partInfo, r io.Reader) error {
		if info.IsAttachment {
			return nil
		}
		media := strings.ToLower(info.Media)
		if media != "text/plain" && media != "text/html" {
			return nil
		}
		body, _ := io.ReadAll(io.LimitReader(decodeTransfer(info.Transfer, r), PreviewBytesLimit))
		body = convertCharset(body, info.Params["charset"])
		if media == "text/plain" && plain == "" {
			plain = string(body)
		} else if media == "text/html" && markup == "" {
			markup = string(body)
		}
		return nil
	}, Limits{MaxDepth: 16, MaxParts: 32})
	return messagepreview.FromBodies(plain, markup)
}
