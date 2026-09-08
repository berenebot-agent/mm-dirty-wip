package mailparse

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
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
	state := walkState{}
	h := textproto.MIMEHeader(msg.Header)
	if err := walkPart(h, msg.Body, &state); err != nil {
		return Parsed{}, err
	}
	p.Text = strings.TrimSpace(strings.Join(state.text, "\n\n"))
	p.HTML = sanitizeHTML(strings.Join(state.html, "\n"))
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

type walkState struct {
	text, html     []string
	attachments    []Attachment
	nextAttachment int
}

func walkPart(h textproto.MIMEHeader, r io.Reader, s *walkState) error {
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
			if err = walkPart(part.Header, part, s); err != nil {
				part.Close()
				return err
			}
			part.Close()
		}
		return nil
	}
	r = decodeTransfer(h.Get("Content-Transfer-Encoding"), r)
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	isAttachment := strings.EqualFold(disp, "attachment") || filename != "" || (!strings.HasPrefix(strings.ToLower(media), "text/") && media != "message/rfc822")
	if isAttachment {
		s.nextAttachment++
		n, err := io.Copy(io.Discard, r)
		if err != nil {
			return err
		}
		s.attachments = append(s.attachments, Attachment{Filename: safeFilename(filename, s.nextAttachment, media), ContentType: media, ContentID: strings.Trim(h.Get("Content-Id"), "<> "), Size: n, PartIndex: s.nextAttachment})
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r, 10<<20))
	if err != nil {
		return err
	}
	body = convertCharset(body, params["charset"])
	switch strings.ToLower(media) {
	case "text/plain":
		s.text = append(s.text, string(body))
	case "text/html":
		s.html = append(s.html, string(body))
	}
	return nil
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
func sanitizeHTML(s string) string { return html.EscapeString(s) }

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
	var walk func(textproto.MIMEHeader, io.Reader) error
	walk = func(h textproto.MIMEHeader, r io.Reader) error {
		ct := h.Get("Content-Type")
		if ct == "" {
			ct = "text/plain"
		}
		media, params, _ := mime.ParseMediaType(ct)
		if strings.HasPrefix(strings.ToLower(media), "multipart/") {
			mr := multipart.NewReader(r, params["boundary"])
			for {
				p, e := mr.NextPart()
				if e == io.EOF {
					break
				}
				if e != nil {
					return e
				}
				e = walk(p.Header, p)
				p.Close()
				if e != nil {
					return e
				}
				if found {
					return nil
				}
			}
			return nil
		}
		disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
		filename := dparams["filename"]
		if filename == "" {
			filename = params["name"]
		}
		isAtt := strings.EqualFold(disp, "attachment") || filename != "" || (!strings.HasPrefix(strings.ToLower(media), "text/") && media != "message/rfc822")
		if !isAtt {
			return nil
		}
		target++
		if target == index {
			_, e := io.Copy(w, decodeTransfer(h.Get("Content-Transfer-Encoding"), r))
			found = e == nil
			return e
		}
		return nil
	}
	if err = walk(textproto.MIMEHeader(msg.Header), msg.Body); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("attachment not found")
	}
	return nil
}

func BuildMessage(from Address, to, cc, bcc []string, subject, text, html, messageID, inReplyTo string, refs []string, date time.Time) ([]byte, error) {
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
	if html == "" {
		fmt.Fprint(w, "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
		qw := quotedprintable.NewWriter(w)
		_, _ = qw.Write([]byte(text))
		_ = qw.Close()
		_ = w.Flush()
		return b.Bytes(), nil
	}
	boundary := "=_oai_" + strings.Trim(strings.ReplaceAll(messageID, "@", "_"), "<>")
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
