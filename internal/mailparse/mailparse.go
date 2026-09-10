package mailparse

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Address struct{ Name, Address string }
type Attachment struct {
	Filename, ContentType, ContentID string
	Content                          []byte
	Size                             int64
	PartIndex                        int
}
type Parsed struct {
	RFCMessageID, InReplyTo string
	References              []string
	From                    Address
	To, CC                  []string
	Subject, Text, HTML     string
	Date                    time.Time
	Attachments             []Attachment
}

func ParseFile(path string) (Parsed, error) {
	f, err := os.Open(path)
	if err != nil {
		return Parsed{}, err
	}
	defer f.Close()
	msg, err := mail.ReadMessage(bufio.NewReader(f))
	if err != nil {
		return Parsed{}, err
	}
	return parseMessage(msg)
}
func parseMessage(msg *mail.Message) (Parsed, error) {
	var p Parsed
	dec := new(mime.WordDecoder)
	p.Subject, _ = dec.DecodeHeader(msg.Header.Get("Subject"))
	p.RFCMessageID = strings.TrimSpace(msg.Header.Get("Message-Id"))
	p.InReplyTo = strings.TrimSpace(msg.Header.Get("In-Reply-To"))
	p.References = strings.Fields(msg.Header.Get("References"))
	if a, err := mail.ParseAddress(msg.Header.Get("From")); err == nil {
		p.From = Address{Name: a.Name, Address: strings.ToLower(a.Address)}
	}
	p.To = parseAddressList(msg.Header, "To")
	p.CC = parseAddressList(msg.Header, "Cc")
	if d, err := msg.Header.Date(); err == nil {
		p.Date = d.UTC()
	} else {
		p.Date = time.Now().UTC()
	}
	state := parseState{}
	if err := walkMIME(textproto.MIMEHeader(msg.Header), msg.Body, state.collect); err != nil {
		return Parsed{}, err
	}
	p.Text = strings.TrimSpace(strings.Join(state.text, "\n\n"))
	p.HTML = strings.Join(state.html, "\n")
	p.Attachments = state.attachments
	return p, nil
}
func parseAddressList(h mail.Header, key string) []string {
	a, err := h.AddressList(key)
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		out = append(out, strings.ToLower(v.Address))
	}
	return out
}

// Limits bound malicious or pathological MIME so parsing stays bounded in
// memory and time.
const (
	maxMIMEDepth = 8
	maxMIMEParts = 256
)

// partInfo is the shared classification of one leaf MIME part.
type partInfo struct {
	Media, Filename, ContentID, Transfer string
	Params                               map[string]string
	IsAttachment                         bool
}

type walker struct {
	fn    func(partInfo, io.Reader) error
	depth int
	parts int
}

// walkMIME performs one bounded traversal of a MIME entity. Container parts are
// recursed; every leaf part is passed to fn with its metadata and a reader at
// the undecoded content. Parsing and extraction share this so their attachment
// ordering and classification can never diverge.
func walkMIME(h textproto.MIMEHeader, r io.Reader, fn func(partInfo, io.Reader) error) error {
	w := &walker{fn: fn}
	return w.walk(h, r)
}

func (w *walker) walk(h textproto.MIMEHeader, r io.Reader) error {
	w.depth++
	defer func() { w.depth-- }()
	if w.depth > maxMIMEDepth {
		return fmt.Errorf("mime nesting too deep")
	}
	w.parts++
	if w.parts > maxMIMEParts {
		return fmt.Errorf("too many mime parts")
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	media, params, err := mime.ParseMediaType(ct)
	if err != nil {
		media = "application/octet-stream"
		params = map[string]string{}
	}
	if strings.HasPrefix(strings.ToLower(media), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart without boundary")
		}
		mr := multipart.NewReader(r, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err = w.walk(part.Header, part); err != nil {
				part.Close()
				return err
			}
			part.Close()
		}
		return nil
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	info := partInfo{
		Media:     media,
		Params:    params,
		Filename:  filename,
		ContentID: strings.Trim(h.Get("Content-Id"), "<> "),
		Transfer:  h.Get("Content-Transfer-Encoding"),
		IsAttachment: strings.EqualFold(disp, "attachment") || filename != "" ||
			(!strings.HasPrefix(strings.ToLower(media), "text/") && media != "message/rfc822"),
	}
	return w.fn(info, r)
}

type parseState struct {
	text, html     []string
	attachments    []Attachment
	nextAttachment int
}

func (s *parseState) collect(info partInfo, r io.Reader) error {
	r = decodeTransfer(info.Transfer, r)
	if info.IsAttachment {
		s.nextAttachment++
		n, err := io.Copy(io.Discard, r)
		if err != nil {
			return err
		}
		s.attachments = append(s.attachments, Attachment{Filename: safeFilename(info.Filename, s.nextAttachment, info.Media), ContentType: info.Media, ContentID: info.ContentID, Size: n, PartIndex: s.nextAttachment})
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r, 10<<20))
	if err != nil {
		return err
	}
	body = convertCharset(body, info.Params["charset"])
	switch strings.ToLower(info.Media) {
	case "text/plain":
		s.text = append(s.text, string(body))
	case "text/html":
		s.html = append(s.html, string(body))
	}
	return nil
}
func SafeAttachmentFilename(name string, index int, media string) string {
	return safeFilename(name, index, media)
}

func safeFilename(name string, index int, media string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name != "" && name != "." {
		return name
	}
	ext := ""
	if exts, _ := mime.ExtensionsByType(media); len(exts) > 0 {
		ext = exts[0]
	}
	return fmt.Sprintf("attachment-%d%s", index, ext)
}
func decodeTransfer(enc string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		return r
	}
}
func convertCharset(b []byte, label string) []byte {
	label = strings.TrimSpace(strings.ToLower(label))
	if label == "" || label == "utf-8" || label == "us-ascii" {
		return b
	}
	switch label {
	case "iso-8859-1", "latin1", "latin-1":
		r := make([]rune, len(b))
		for i, v := range b {
			r[i] = rune(v)
		}
		return []byte(string(r))
	case "windows-1252", "cp1252":
		r := make([]rune, 0, len(b))
		for _, v := range b {
			if v >= 0x80 && v <= 0x9f {
				r = append(r, windows1252Rune(v))
			} else {
				r = append(r, rune(v))
			}
		}
		return []byte(string(r))
	default:
		return b
	}
}
func windows1252Rune(b byte) rune {
	table := map[byte]rune{0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ', 0x89: '‰', 0x8a: 'Š', 0x8b: '‹', 0x8c: 'Œ', 0x8e: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—', 0x98: '˜', 0x99: '™', 0x9a: 'š', 0x9b: '›', 0x9c: 'œ', 0x9e: 'ž', 0x9f: 'Ÿ'}
	if r, ok := table[b]; ok {
		return r
	}
	return rune(b)
}

// errWalkStop lets a callback end the traversal early once its target is found.
var errWalkStop = errors.New("mime walk stopped")

func ExtractAttachment(path string, index int, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	msg, err := mail.ReadMessage(bufio.NewReader(f))
	if err != nil {
		return err
	}
	target := 0
	found := false
	err = walkMIME(textproto.MIMEHeader(msg.Header), msg.Body, func(info partInfo, r io.Reader) error {
		if !info.IsAttachment {
			return nil
		}
		target++
		if target != index {
			return nil
		}
		if _, err := io.Copy(w, decodeTransfer(info.Transfer, r)); err != nil {
			return err
		}
		found = true
		return errWalkStop
	})
	if err != nil && !errors.Is(err, errWalkStop) {
		return err
	}
	if !found {
		return fmt.Errorf("attachment not found")
	}
	return nil
}

// ExtractAllAttachments extracts every attachment in one traversal. The
// callback must consume the reader before returning.
func ExtractAllAttachments(path string, fn func(Attachment, io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	msg, err := mail.ReadMessage(bufio.NewReader(f))
	if err != nil {
		return err
	}
	target := 0
	return walkMIME(textproto.MIMEHeader(msg.Header), msg.Body, func(info partInfo, r io.Reader) error {
		if !info.IsAttachment {
			return nil
		}
		target++
		meta := Attachment{Filename: safeFilename(info.Filename, target, info.Media), ContentType: info.Media, ContentID: info.ContentID, PartIndex: target}
		return fn(meta, decodeTransfer(info.Transfer, r))
	})
}

// HasControlChars reports whether s contains a CR, LF, or another C0/DEL
// control character. Header values must never contain them: they enable MIME
// header injection (smuggling extra headers or body content into a message).
func HasControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// boundaryFor derives a MIME boundary from a message id. MIME boundary tokens
// must not contain characters such as '@', '<' or '>'.
func boundaryFor(prefix, messageID string) string {
	return prefix + strings.Trim(strings.ReplaceAll(messageID, "@", "_"), "<>")
}

// validateHeaderField rejects values that would break out of a single header
// line. It is a defense-in-depth companion to address validation at the API
// boundary.
func validateHeaderField(name, value string) error {
	if HasControlChars(value) {
		return fmt.Errorf("%s contains control characters", name)
	}
	return nil
}

func BuildMessage(from Address, to, cc, bcc []string, subject, text, html, messageID, inReplyTo string, refs []string, date time.Time, attachments []Attachment) ([]byte, error) {
	if err := validateHeaderField("from name", from.Name); err != nil {
		return nil, err
	}
	if err := validateHeaderField("from address", from.Address); err != nil {
		return nil, err
	}
	for _, addr := range to {
		if err := validateHeaderField("to address", addr); err != nil {
			return nil, err
		}
	}
	for _, addr := range cc {
		if err := validateHeaderField("cc address", addr); err != nil {
			return nil, err
		}
	}
	for _, addr := range bcc {
		if err := validateHeaderField("bcc address", addr); err != nil {
			return nil, err
		}
	}
	for _, value := range append([]string{messageID, inReplyTo}, refs...) {
		if err := validateHeaderField("message reference", value); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	fromHeader := from.Address
	if from.Name != "" {
		fromHeader = (&mail.Address{Name: from.Name, Address: from.Address}).String()
	}
	fmt.Fprintf(w, "From: %s\r\n", fromHeader)
	fmt.Fprintf(w, "To: %s\r\n", strings.Join(to, ", "))
	if len(cc) > 0 {
		fmt.Fprintf(w, "Cc: %s\r\n", strings.Join(cc, ", "))
	}
	// BCC recipients are deliberately NOT written into the generated MIME.
	// They are carried only in the provider/SMTP envelope so recipients never
	// see each other's addresses.
	fmt.Fprintf(w, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(w, "Date: %s\r\n", date.Format(time.RFC1123Z))
	fmt.Fprintf(w, "Message-ID: %s\r\n", messageID)
	if inReplyTo != "" {
		fmt.Fprintf(w, "In-Reply-To: %s\r\n", inReplyTo)
	}
	if len(refs) > 0 {
		fmt.Fprintf(w, "References: %s\r\n", strings.Join(refs, " "))
	}
	fmt.Fprint(w, "MIME-Version: 1.0\r\n")
	if len(attachments) == 0 {
		if html == "" {
			fmt.Fprint(w, "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
			qw := quotedprintable.NewWriter(w)
			_, _ = qw.Write([]byte(text))
			_ = qw.Close()
			_ = w.Flush()
			return b.Bytes(), nil
		}
		boundary := boundaryFor("=_ghm_", messageID)
		fmt.Fprintf(w, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
		fmt.Fprintf(w, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary)
		qw := quotedprintable.NewWriter(w)
		_, _ = qw.Write([]byte(text))
		_ = qw.Close()
		fmt.Fprintf(w, "\r\n--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary)
		qw = quotedprintable.NewWriter(w)
		_, _ = qw.Write([]byte(html))
		_ = qw.Close()
		fmt.Fprintf(w, "\r\n--%s--\r\n", boundary)
		_ = w.Flush()
		return b.Bytes(), nil
	}
	outerBoundary := boundaryFor("=_ghm_mix_", messageID)
	fmt.Fprintf(w, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", outerBoundary)
	if err := w.Flush(); err != nil {
		return nil, err
	}
	outer := multipart.NewWriter(&b)
	if err := outer.SetBoundary(outerBoundary); err != nil {
		return nil, err
	}
	if html != "" {
		altBoundary := boundaryFor("=_ghm_alt_", messageID)
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", altBoundary))
		part, err := outer.CreatePart(h)
		if err != nil {
			return nil, err
		}
		alt := multipart.NewWriter(part)
		if err = alt.SetBoundary(altBoundary); err != nil {
			return nil, err
		}
		if err = writeTextPart(alt, "text/plain; charset=utf-8", text); err != nil {
			return nil, err
		}
		if err = writeTextPart(alt, "text/html; charset=utf-8", html); err != nil {
			return nil, err
		}
		if err = alt.Close(); err != nil {
			return nil, err
		}
	} else if err := writeTextPart(outer, "text/plain; charset=utf-8", text); err != nil {
		return nil, err
	}
	for index, attachment := range attachments {
		filename := safeFilename(attachment.Filename, index+1, attachment.ContentType)
		contentType := attachment.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", mime.FormatMediaType(contentType, map[string]string{"name": filename}))
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		h.Set("Content-Transfer-Encoding", "base64")
		if attachment.ContentID != "" {
			h.Set("Content-ID", "<"+strings.Trim(attachment.ContentID, "<>")+">")
		}
		part, err := outer.CreatePart(h)
		if err != nil {
			return nil, err
		}
		encoder := base64.NewEncoder(base64.StdEncoding, part)
		if _, err = encoder.Write(attachment.Content); err != nil {
			return nil, err
		}
		if err = encoder.Close(); err != nil {
			return nil, err
		}
	}
	if err := outer.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeTextPart(w *multipart.Writer, contentType, body string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "quoted-printable")
	part, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	qw := quotedprintable.NewWriter(part)
	if _, err = qw.Write([]byte(body)); err != nil {
		return err
	}
	return qw.Close()
}
