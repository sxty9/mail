// Package message builds and parses RFC 5322 internet messages for maild. It is the
// single place that knows the on-the-wire format: the UI/JMAP layers work with the
// structured Headers/Parsed types, never raw bytes. Uses the stdlib (net/mail + mime) plus
// golang.org/x/text for charset decoding (legacy encodings like Windows-1252/ISO-8859-* →
// UTF-8) so non-ASCII bodies render correctly.
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
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/transform"
)

// maxBody caps how much of a single message/part we will read into memory.
const maxBody = 25 << 20 // 25 MiB

// Headers is the lightweight summary used for message lists (no body decoding). HasAttachments
// is a cheap headers-only heuristic (a top-level multipart/mixed) for the list view's paperclip;
// the authoritative attachment list comes from a full Parse.
type Headers struct {
	From           string    `json:"from"`
	To             string    `json:"to"`
	Cc             string    `json:"cc"`
	Subject        string    `json:"subject"`
	Date           time.Time `json:"date"`
	MessageID      string    `json:"messageId"`
	InReplyTo      string    `json:"inReplyTo"`
	References     []string  `json:"references"`
	HasAttachments bool      `json:"hasAttachments"`
}

// Attachment is a MIME attachment: metadata plus the decoded bytes. On parse, Data holds the
// decoded content; in build input, the caller supplies Filename/ContentType/Data. Data is
// excluded from JSON (list views ship only metadata; downloads stream the bytes separately).
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	ContentID   string `json:"contentId,omitempty"`
	Inline      bool   `json:"inline,omitempty"`
	Data        []byte `json:"-"`
}

// Parsed is a full message: headers, the extracted text/html bodies, and any attachments.
type Parsed struct {
	Headers
	Text        string       `json:"text"`
	HTML        string       `json:"html"`
	Attachments []Attachment `json:"attachments"`
	// Calendar holds a decoded text/calendar part (iMIP), if any; CalendarMethod is its iTIP
	// method (in-body METHOD wins over the MIME method= param). Empty for ordinary mail.
	Calendar       string `json:"calendar,omitempty"`
	CalendarMethod string `json:"calendarMethod,omitempty"`
}

// BuildOptions describes a message to compose.
type BuildOptions struct {
	From       string // bare address, e.g. user@domain
	FromName   string // optional display name → "FromName <From>"
	To         []string
	Cc         []string
	Subject    string
	Body       string // plain text (UTF-8)
	HTMLBody   string // optional rich-text HTML body; when set, Build emits multipart/alternative
	InReplyTo  string
	References []string
	Date       time.Time
	Domain     string       // used for the Message-ID right-hand side
	Attachments []Attachment // optional; triggers multipart/mixed
	// CalendarICS, when set, makes Build emit a multipart/alternative (text/plain +
	// text/calendar) iMIP message (RFC 6047). CalendarMethod is the iTIP method
	// (REQUEST/REPLY/CANCEL); it defaults to REQUEST. Used by the icaly calendar service.
	CalendarICS    string
	CalendarMethod string
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

// Build renders a message and returns the raw bytes plus the assigned Message-ID. The structure
// is the simplest that fits the inputs: text/plain for a plain body; multipart/alternative when an
// HTML body is present (text + html); multipart/mixed when attachments are present (wrapping the
// body, which is itself a multipart/alternative if HTML is present).
func Build(o BuildOptions) ([]byte, string) {
	if o.Date.IsZero() {
		o.Date = time.Now()
	}
	msgID := NewMessageID(o.Domain)
	var b strings.Builder
	// hdr writes one unfolded header. stripCRLF is the last line of defence against header
	// injection: any CR/LF smuggled into a user-controlled value (Subject, To, Cc, In-Reply-To,
	// References) is neutralised here so it can never start a forged header or a second body.
	hdr := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", k, stripCRLF(v))
		}
	}
	fromHeader := o.From
	if o.FromName != "" && o.From != "" {
		fromHeader = (&mail.Address{Name: o.FromName, Address: o.From}).String()
	}
	hdr("Date", o.Date.Format(time.RFC1123Z))
	hdr("From", fromHeader)
	hdr("To", strings.Join(o.To, ", "))
	hdr("Cc", strings.Join(o.Cc, ", "))
	hdr("Subject", encodeWord(o.Subject))
	hdr("Message-ID", msgID)
	hdr("In-Reply-To", o.InReplyTo)
	if len(o.References) > 0 {
		hdr("References", strings.Join(o.References, " "))
	}
	hdr("MIME-Version", "1.0")

	// iMIP (RFC 6047): a multipart/alternative carrying the text body and the calendar object.
	// The text/calendar part is base64-encoded so its CRLF line folding survives transit intact.
	if o.CalendarICS != "" {
		method := strings.ToUpper(strings.TrimSpace(o.CalendarMethod))
		if method == "" {
			method = "REQUEST"
		}
		var mbuf bytes.Buffer
		mw := multipart.NewWriter(&mbuf)
		boundary := mw.Boundary()
		th := textproto.MIMEHeader{}
		th.Set("Content-Type", "text/plain; charset=utf-8")
		th.Set("Content-Transfer-Encoding", "quoted-printable")
		if tp, err := mw.CreatePart(th); err == nil {
			qw := quotedprintable.NewWriter(tp)
			_, _ = qw.Write([]byte(normalizeNewlines(o.Body)))
			_ = qw.Close()
		}
		ch := textproto.MIMEHeader{}
		ch.Set("Content-Type", fmt.Sprintf("text/calendar; charset=utf-8; method=%s; component=VEVENT", method))
		ch.Set("Content-Transfer-Encoding", "base64")
		if cp, err := mw.CreatePart(ch); err == nil {
			writeBase64(cp, []byte(o.CalendarICS))
		}
		_ = mw.Close()
		hdr("Content-Class", "urn:content-classes:calendarmessage")
		hdr("Content-Type", formatMedia("multipart/alternative", "boundary", boundary))
		b.WriteString("\r\n")
		return append([]byte(b.String()), mbuf.Bytes()...), msgID
	}

	// Sanitise any HTML body (defence-in-depth — the editor is trusted but an app-password client
	// could POST arbitrary HTML), and derive a plain-text alternative when the caller gave none.
	html := sanitizeHTMLBody(o.HTMLBody)
	text := o.Body
	if strings.TrimSpace(text) == "" && html != "" {
		text = htmlToText(html)
	}
	hasHTML := html != ""
	hasAtt := len(o.Attachments) > 0

	// Plain text only: the simple 8bit text/plain path.
	if !hasHTML && !hasAtt {
		hdr("Content-Type", "text/plain; charset=utf-8")
		hdr("Content-Transfer-Encoding", "8bit")
		b.WriteString("\r\n")
		body := normalizeNewlines(text)
		b.WriteString(body)
		if !strings.HasSuffix(body, "\r\n") {
			b.WriteString("\r\n")
		}
		return []byte(b.String()), msgID
	}

	// Formatted mail, no attachments: multipart/alternative (text + html).
	if hasHTML && !hasAtt {
		ct, body := renderAlternative(text, html)
		hdr("Content-Type", ct)
		b.WriteString("\r\n")
		return append([]byte(b.String()), body...), msgID
	}

	// With attachments: multipart/mixed wrapping the body (a text/plain part, or a nested
	// multipart/alternative when HTML is present) followed by base64 attachment parts.
	var mbuf bytes.Buffer
	mw := multipart.NewWriter(&mbuf)
	boundary := mw.Boundary()
	if hasHTML {
		ct, altBytes := renderAlternative(text, html)
		ah := textproto.MIMEHeader{}
		ah.Set("Content-Type", ct)
		if part, err := mw.CreatePart(ah); err == nil {
			_, _ = part.Write(altBytes)
		}
	} else {
		th := textproto.MIMEHeader{}
		th.Set("Content-Type", "text/plain; charset=utf-8")
		th.Set("Content-Transfer-Encoding", "quoted-printable")
		if tp, err := mw.CreatePart(th); err == nil {
			qw := quotedprintable.NewWriter(tp)
			_, _ = qw.Write([]byte(normalizeNewlines(text)))
			_ = qw.Close()
		}
	}
	for _, a := range o.Attachments {
		mt := bareMediaType(a.ContentType)
		ah := textproto.MIMEHeader{}
		ah.Set("Content-Type", formatMedia(mt, "name", a.Filename))
		ah.Set("Content-Transfer-Encoding", "base64")
		ah.Set("Content-Disposition", formatMedia("attachment", "filename", a.Filename))
		if ap, err := mw.CreatePart(ah); err == nil {
			writeBase64(ap, a.Data)
		}
	}
	_ = mw.Close()
	hdr("Content-Type", formatMedia("multipart/mixed", "boundary", boundary))
	b.WriteString("\r\n")
	return append([]byte(b.String()), mbuf.Bytes()...), msgID
}

// Summary parses only the headers (cheap — the body reader is never consumed).
func Summary(raw []byte) (Headers, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Headers{}, err
	}
	return headersOf(m.Header), nil
}

// Parse parses headers and extracts text/html bodies + attachments (decoding transfer
// encodings, converting legacy charsets to UTF-8, and walking multipart trees).
func Parse(raw []byte) (Parsed, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Parsed{}, err
	}
	p := Parsed{Headers: headersOf(m.Header)}
	body, _ := io.ReadAll(io.LimitReader(m.Body, maxBody))
	var acc parseAcc
	walk(textproto.MIMEHeader(m.Header), body, 0, &acc)
	p.Text = acc.text.String()
	p.HTML = acc.html.String()
	p.Attachments = acc.attachments
	p.Calendar = acc.calendar
	p.CalendarMethod = acc.calMethod
	if p.Text == "" && p.HTML != "" {
		p.Text = stripHTML(p.HTML)
	}
	return p, nil
}

func headersOf(h mail.Header) Headers {
	dec := new(mime.WordDecoder)
	out := Headers{
		From:           decodeWord(dec, h.Get("From")),
		To:             decodeWord(dec, h.Get("To")),
		Cc:             decodeWord(dec, h.Get("Cc")),
		Subject:        decodeWord(dec, h.Get("Subject")),
		MessageID:      strings.TrimSpace(h.Get("Message-ID")),
		InReplyTo:      strings.TrimSpace(h.Get("In-Reply-To")),
		References:     strings.Fields(h.Get("References")),
		HasAttachments: hasAttachmentHint(h.Get("Content-Type")),
	}
	if d, err := mail.ParseDate(h.Get("Date")); err == nil {
		out.Date = d
	}
	return out
}

// hasAttachmentHint reports whether a top-level Content-Type implies attachments without
// decoding the body: multipart/mixed is the standard container, and a single-part message whose
// top-level type is itself a non-text payload (e.g. a bare application/pdf) IS an attachment.
// multipart/alternative (text+html) is deliberately excluded to avoid a paperclip on every
// formatted mail; the reading pane's full parse remains authoritative.
func hasAttachmentHint(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || mt == "" {
		return false
	}
	if mt == "multipart/mixed" {
		return true
	}
	return !strings.HasPrefix(mt, "multipart/") && !strings.HasPrefix(mt, "text/")
}

type parseAcc struct {
	text        strings.Builder
	html        strings.Builder
	attachments []Attachment
	calendar    string // first text/calendar part, decoded to UTF-8
	calMethod   string // its iTIP METHOD (in-body wins over the MIME method= param)
}

// walk decodes a MIME node into acc: text/plain → text, text/html → html, anything else
// (or any part with an attachment disposition / filename) → attachments. Recurses into
// multipart/* containers.
func walk(hdr textproto.MIMEHeader, data []byte, depth int, acc *parseAcc) {
	if depth > 12 {
		return
	}
	mediatype, params, err := mime.ParseMediaType(hdr.Get("Content-Type"))
	if err != nil || mediatype == "" {
		mediatype, params = "text/plain", map[string]string{}
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
			walk(part.Header, pdata, depth+1, acc)
		}
	}

	cte := hdr.Get("Content-Transfer-Encoding")
	disp, dparams, _ := mime.ParseMediaType(hdr.Get("Content-Disposition"))
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	isText := strings.HasPrefix(mediatype, "text/plain") || strings.HasPrefix(mediatype, "text/html")

	// iMIP calendar part (RFC 6047): capture it as the structured calendar payload AND keep a
	// downloadable attachment copy. The in-body METHOD wins over the MIME method= param.
	if strings.HasPrefix(mediatype, "text/calendar") || mediatype == "application/ics" {
		decoded := toUTF8(params["charset"], decodeCTE(cte, data))
		if acc.calendar == "" {
			acc.calendar = string(decoded)
		}
		if m := icalMethod(decoded); m != "" {
			acc.calMethod = m
		} else if mp := strings.ToUpper(strings.TrimSpace(params["method"])); mp != "" && acc.calMethod == "" {
			acc.calMethod = mp
		}
		name := decodeWord(new(mime.WordDecoder), filename)
		if name == "" {
			name = "invite.ics"
		}
		acc.attachments = append(acc.attachments, Attachment{
			Filename: name, ContentType: "text/calendar", Size: len(decoded), Inline: disp == "inline", Data: decoded,
		})
		return
	}

	// Attachment when: explicit attachment disposition, a named non-text part, or any
	// non-text leaf (e.g. an unnamed inline image). Text bodies fall through to the builders.
	if disp == "attachment" || (filename != "" && !isText) || !isText {
		decoded := decodeCTE(cte, data)
		dec := new(mime.WordDecoder)
		acc.attachments = append(acc.attachments, Attachment{
			Filename:    decodeWord(dec, filename),
			ContentType: mediatype,
			Size:        len(decoded),
			ContentID:   strings.Trim(strings.TrimSpace(hdr.Get("Content-ID")), "<>"),
			Inline:      disp == "inline",
			Data:        decoded,
		})
		return
	}

	decoded := decodeCTE(cte, data)
	switch {
	case strings.HasPrefix(mediatype, "text/html"):
		acc.html.Write(toUTF8(params["charset"], decoded))
	case strings.HasPrefix(mediatype, "text/plain"):
		acc.text.Write(toUTF8(params["charset"], decoded))
	}
}

// icalMethod returns the uppercased value of a top-level METHOD: property in an iCalendar
// body, or "" if absent. Authoritative over the MIME method= param (which can be wrong).
func icalMethod(b []byte) string {
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if u := strings.ToUpper(strings.TrimSpace(line)); strings.HasPrefix(u, "METHOD:") {
			return strings.TrimSpace(u[len("METHOD:"):])
		}
	}
	return ""
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

// toUTF8 converts a body part from its declared charset to UTF-8. UTF-8/ASCII pass through;
// legacy single- and multi-byte charsets are decoded via x/text; unknown charsets are left
// as-is (best effort).
func toUTF8(charset string, data []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii", "unicode-1-1-utf-8":
		return data
	}
	enc, err := ianaindex.MIME.Encoding(charset)
	if err != nil || enc == nil {
		if enc, err = ianaindex.IANA.Encoding(charset); err != nil || enc == nil {
			return data
		}
	}
	out, _, err := transform.Bytes(enc.NewDecoder(), data)
	if err != nil || len(out) == 0 {
		return data
	}
	return out
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

// bareMediaType strips any parameters from a content-type, defaulting unknowns to octet-stream.
func bareMediaType(ct string) string {
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt != "" {
		return mt
	}
	return "application/octet-stream"
}

// formatMedia builds a header value with one parameter, RFC 2231-encoding non-ASCII values.
func formatMedia(base, key, val string) string {
	if val == "" {
		return base
	}
	if v := mime.FormatMediaType(base, map[string]string{key: val}); v != "" {
		return v
	}
	return base
}

func writeBase64(w io.Writer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(enc); i += 76 {
		end := i + 76
		if end > len(enc) {
			end = len(enc)
		}
		_, _ = io.WriteString(w, enc[i:end])
		_, _ = io.WriteString(w, "\r\n")
	}
}

// renderAlternative builds a multipart/alternative body carrying the plain-text part first and the
// HTML part second (RFC 2046 order: least-to-most faithful), both quoted-printable. It returns the
// Content-Type header value (with the boundary) and the raw body bytes.
func renderAlternative(text, html string) (string, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	boundary := mw.Boundary()
	th := textproto.MIMEHeader{}
	th.Set("Content-Type", "text/plain; charset=utf-8")
	th.Set("Content-Transfer-Encoding", "quoted-printable")
	if tp, err := mw.CreatePart(th); err == nil {
		qw := quotedprintable.NewWriter(tp)
		_, _ = qw.Write([]byte(normalizeNewlines(text)))
		_ = qw.Close()
	}
	hh := textproto.MIMEHeader{}
	hh.Set("Content-Type", "text/html; charset=utf-8")
	hh.Set("Content-Transfer-Encoding", "quoted-printable")
	if hp, err := mw.CreatePart(hh); err == nil {
		qw := quotedprintable.NewWriter(hp)
		_, _ = qw.Write([]byte(normalizeNewlines(html)))
		_ = qw.Close()
	}
	_ = mw.Close()
	return formatMedia("multipart/alternative", "boundary", boundary), buf.Bytes()
}

// Outgoing-HTML sanitiser patterns, compiled once. Go's RE2 has no backreferences, so the
// content-bearing elements (script/style) are matched explicitly and the remaining structural
// tags are stripped open-or-close generically. The on* handler patterns accept a slash or quote
// (not just whitespace) before the handler name, because HTML5 allows `<img/onerror=…>` and
// `<img src="x"onerror=…>`.
var (
	reHTMLComment = regexp.MustCompile(`(?is)<!--.*?-->`)
	reHTMLScript  = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`)
	reHTMLStyle   = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`)
	reHTMLDanger  = regexp.MustCompile(`(?is)</?\s*(script|style|iframe|object|embed|applet|form|link|meta|base|noscript|frame|frameset)\b[^>]*>`)
	reHTMLOnDq    = regexp.MustCompile(`(?i)[\s/"']on[a-z]+\s*=\s*"[^"]*"`)
	reHTMLOnSq    = regexp.MustCompile(`(?i)[\s/"']on[a-z]+\s*=\s*'[^']*'`)
	reHTMLOnBare  = regexp.MustCompile(`(?i)[\s/"']on[a-z]+\s*=\s*[^\s>]+`)
	reHTMLBr      = regexp.MustCompile(`(?i)<br\s*/?>`)
	reHTMLBlock   = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6])>`)
	// URL-bearing attributes; the value is checked after decoding entities and stripping
	// whitespace/control chars, so obfuscated schemes (java&#9;script:, java\tscript:) are caught.
	reURLAttr   = regexp.MustCompile(`(?i)(href|src|xlink:href|background|action)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	reNumEntity = regexp.MustCompile(`(?i)&#(x?[0-9a-f]+);?`)
)

// sanitizeHTMLBody strips active/dangerous content from an OUTGOING HTML body: scripts, embedded
// objects, document-structure elements, inline event handlers and javascript:/vbscript: URLs. It
// keeps formatting tags and inline styles so the rich-text body renders faithfully at the
// recipient. Returns "" for an effectively empty body (so Build falls back to text/plain).
func sanitizeHTMLBody(html string) string {
	if strings.TrimSpace(html) == "" {
		return ""
	}
	// Run the strip passes to a fixpoint: removing an interposed tag (e.g. "<scr<iframe>ipt>") can
	// reconstitute a dangerous tag, so re-scan until the string stops changing. Each pass only
	// removes/shortens, so this converges; the cap is a belt-and-braces bound.
	out := html
	for i := 0; i < 64; i++ {
		prev := out
		out = reHTMLComment.ReplaceAllString(out, "")
		out = reHTMLScript.ReplaceAllString(out, "")
		out = reHTMLStyle.ReplaceAllString(out, "")
		out = reHTMLDanger.ReplaceAllString(out, "")
		out = reHTMLOnDq.ReplaceAllString(out, "")
		out = reHTMLOnSq.ReplaceAllString(out, "")
		out = reHTMLOnBare.ReplaceAllString(out, "")
		out = neutralizeDangerURLs(out)
		if out == prev {
			break
		}
	}
	if strings.TrimSpace(stripHTML(out)) == "" && !strings.Contains(strings.ToLower(out), "<img") {
		return "" // nothing visible survived → treat as no HTML body
	}
	return out
}

// neutralizeDangerURLs rewrites any href/src/etc whose value resolves to a dangerous scheme to "#",
// resilient to whitespace-, control-char- and entity-obfuscated schemes.
func neutralizeDangerURLs(html string) string {
	return reURLAttr.ReplaceAllStringFunc(html, func(m string) string {
		sub := reURLAttr.FindStringSubmatch(m)
		attr, dq, sq, bare := sub[1], sub[2], sub[3], sub[4]
		val := dq + sq + bare
		if !dangerousURL(val) {
			return m
		}
		switch {
		case bare != "":
			return attr + "=#"
		case sq != "":
			return attr + "='#'"
		default:
			return attr + `="#"`
		}
	})
}

// dangerousURL reports whether a URL attribute value, once decoded and de-obfuscated, begins with a
// script-executing or inline-HTML scheme.
func dangerousURL(val string) bool {
	v := reNumEntity.ReplaceAllStringFunc(val, func(e string) string {
		sm := reNumEntity.FindStringSubmatch(e)
		num := sm[1]
		var code int64
		var err error
		if len(num) > 0 && (num[0] == 'x' || num[0] == 'X') {
			code, err = strconv.ParseInt(num[1:], 16, 32)
		} else {
			code, err = strconv.ParseInt(num, 10, 32)
		}
		if err != nil || code <= 0 || code > 0x10FFFF {
			return ""
		}
		return string(rune(code))
	})
	var b strings.Builder
	for _, r := range v {
		if r <= 0x20 { // drop spaces and all ASCII control characters
			continue
		}
		b.WriteRune(r)
	}
	low := strings.ToLower(b.String())
	return strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "vbscript:") || strings.HasPrefix(low, "data:text/html")
}

// htmlToText derives a plain-text fallback from an HTML body: block-level tags become newlines,
// remaining tags are stripped, and the common entities are unescaped. Used only when the caller
// supplies no plain-text body of its own.
func htmlToText(html string) string {
	s := reHTMLBr.ReplaceAllString(html, "\n")
	s = reHTMLBlock.ReplaceAllString(s, "\n")
	s = stripHTML(s)
	r := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&apos;", "'")
	return strings.TrimSpace(r.Replace(s))
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

// stripCRLF replaces any carriage return or line feed with a space. Build emits only unfolded
// single-line headers, so a CR/LF in a header value can only be an injection attempt.
func stripCRLF(s string) string {
	if strings.IndexAny(s, "\r\n") < 0 {
		return s
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// stripHTML is a deliberately naive tag remover, used only as a plain-text fallback when a
// message carries an HTML body but no text/plain alternative. It is not a sanitizer.
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
