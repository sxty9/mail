// Package message builds and parses RFC 5322 internet messages for maild. It is the
// single place that knows the on-the-wire format: the UI/JMAP layers work with the
// structured Headers/Parsed types, never raw bytes. Pure stdlib (net/mail + mime), so
// the daemon stays a single-binary, CGO-free build like the other holistic services.
package message

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// maxBody caps how much of a single message/part we will read into memory.
const maxBody = 25 << 20 // 25 MiB

// Headers is the lightweight summary used for message lists (no body decoding).
type Headers struct {
	From       string    `json:"from"`
	To         string    `json:"to"`
	Cc         string    `json:"cc"`
	Subject    string    `json:"subject"`
	Date       time.Time `json:"date"`
	MessageID  string    `json:"messageId"`
	InReplyTo  string    `json:"inReplyTo"`
	References []string  `json:"references"`
}

// Parsed is a full message: headers plus the extracted text/html bodies.
type Parsed struct {
	Headers
	Text string `json:"text"`
	HTML string `json:"html"`
}

// BuildOptions describes a message to compose.
type BuildOptions struct {
	From       string
	To         []string
	Cc         []string
	Subject    string
	Body       string // plain text (UTF-8)
	InReplyTo  string
	References []string
	Date       time.Time
	Domain     string // used for the Message-ID right-hand side
}

// NewMessageID returns a globally-unique RFC 5322 Message-ID.
func NewMessageID(domain string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	if domain == "" {
		domain = "localhost"
	}
	return fmt.Sprintf("<%s.%d@%s>", hex.EncodeToString(b[:]), time.Now().UnixNano(), domain)
}

// Build renders a text/plain message and returns the raw bytes plus the Message-ID it
// assigned (so callers can record it for threading).
func Build(o BuildOptions) ([]byte, string) {
	if o.Date.IsZero() {
		o.Date = time.Now()
	}
	msgID := NewMessageID(o.Domain)
	var b strings.Builder
	hdr := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	hdr("Date", o.Date.Format(time.RFC1123Z))
	hdr("From", o.From)
	hdr("To", strings.Join(o.To, ", "))
	hdr("Cc", strings.Join(o.Cc, ", "))
	hdr("Subject", encodeWord(o.Subject))
	hdr("Message-ID", msgID)
	hdr("In-Reply-To", o.InReplyTo)
	if len(o.References) > 0 {
		hdr("References", strings.Join(o.References, " "))
	}
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	body := normalizeNewlines(o.Body)
	b.WriteString(body)
	if !strings.HasSuffix(body, "\r\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String()), msgID
}

// Summary parses only the headers (cheap — the body reader is never consumed). Used to
// build message lists without decoding every body.
func Summary(raw []byte) (Headers, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Headers{}, err
	}
	return headersOf(m.Header), nil
}

// Parse parses headers and extracts the text/html bodies (decoding transfer encodings
// and walking multipart trees).
func Parse(raw []byte) (Parsed, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Parsed{}, err
	}
	p := Parsed{Headers: headersOf(m.Header)}
	var text, html strings.Builder
	body, _ := io.ReadAll(io.LimitReader(m.Body, maxBody))
	walk(m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), body, 0, &text, &html)
	p.Text = text.String()
	p.HTML = html.String()
	if p.Text == "" && p.HTML != "" {
		p.Text = stripHTML(p.HTML)
	}
	return p, nil
}

func headersOf(h mail.Header) Headers {
	dec := new(mime.WordDecoder)
	out := Headers{
		From:       decodeWord(dec, h.Get("From")),
		To:         decodeWord(dec, h.Get("To")),
		Cc:         decodeWord(dec, h.Get("Cc")),
		Subject:    decodeWord(dec, h.Get("Subject")),
		MessageID:  strings.TrimSpace(h.Get("Message-ID")),
		InReplyTo:  strings.TrimSpace(h.Get("In-Reply-To")),
		References: strings.Fields(h.Get("References")),
	}
	if d, err := mail.ParseDate(h.Get("Date")); err == nil {
		out.Date = d
	}
	return out
}

// walk decodes a MIME node, appending text/plain to text and text/html to html. It
// recurses into multipart/* containers (alternative, mixed, related, …).
func walk(ctype, cte string, data []byte, depth int, text, html *strings.Builder) {
	if depth > 12 {
		return
	}
	mediatype, params, err := mime.ParseMediaType(ctype)
	if err != nil || mediatype == "" {
		mediatype = "text/plain"
	}
	if strings.HasPrefix(mediatype, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		mr := multipart.NewReader(bytes.NewReader(data), boundary)
		for {
			part, err := mr.NextPart()
			if err != nil {
				return
			}
			pdata, _ := io.ReadAll(io.LimitReader(part, maxBody))
			walk(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), pdata, depth+1, text, html)
		}
	}
	decoded := decodeCTE(cte, data)
	switch {
	case strings.HasPrefix(mediatype, "text/html"):
		html.Write(decoded)
	case strings.HasPrefix(mediatype, "text/plain"):
		text.Write(decoded)
	}
}

func decodeCTE(cte string, data []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		if out, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(data)), "")); err == nil {
			return out
		}
	case "quoted-printable":
		if out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(data))); err == nil {
			return out
		}
	}
	return data
}

func encodeWord(s string) string {
	for _, r := range s {
		if r > 127 {
			return mime.QEncoding.Encode("utf-8", s)
		}
	}
	return s
}

func decodeWord(dec *mime.WordDecoder, s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if out, err := dec.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// stripHTML is a deliberately naive tag remover, used only as a plain-text fallback when a
// message carries an HTML body but no text/plain alternative (the UI renders text, never
// raw HTML). It is not a sanitizer.
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(collapseBlankLines(b.String()))
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 2 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, strings.TrimRight(ln, " \t\r"))
	}
	return strings.Join(out, "\n")
}
