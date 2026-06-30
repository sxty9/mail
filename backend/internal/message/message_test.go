package message

import (
	"strings"
	"testing"
)

// TestCharsetWindows1252 is the regression test for the "f<U+FFFD>r" bug: a body declared
// windows-1252 with a raw 0xFC byte (ü) must decode to UTF-8 "für", not the replacement char.
func TestCharsetWindows1252(t *testing.T) {
	raw := []byte("Subject: t\r\nContent-Type: text/plain; charset=windows-1252\r\n\r\nGesendet von Outlook f\xfcr Mac\r\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(p.Text, "für") {
		t.Errorf("text = %q, want it to contain %q", p.Text, "für")
	}
	if strings.ContainsRune(p.Text, '�') {
		t.Errorf("text still contains the replacement char: %q", p.Text)
	}
}

// TestQuotedPrintableLatin1 covers CTE + charset together: ISO-8859-1 quoted-printable =FC → ü.
func TestQuotedPrintableLatin1(t *testing.T) {
	raw := []byte("Subject: t\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nGr=FC=DFe\r\n")
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(p.Text, "Grüße") {
		t.Errorf("text = %q, want %q", p.Text, "Grüße")
	}
}

// TestBuildFromDisplayName: a FromName must render as "Name <addr>".
func TestBuildFromDisplayName(t *testing.T) {
	raw, _ := Build(BuildOptions{
		From:     "nanu@henrysoase.org",
		FromName: "Henry Firlay",
		To:       []string{"x@y.test"},
		Subject:  "hi",
		Body:     "body",
		Domain:   "henrysoase.org",
	})
	s := string(raw)
	if !strings.Contains(s, "Henry Firlay") || !strings.Contains(s, "<nanu@henrysoase.org>") {
		t.Errorf("From header missing display name: %q", firstHeader(s, "From"))
	}
}

// TestAttachmentRoundTrip: Build with an attachment, then Parse recovers body + attachment.
func TestAttachmentRoundTrip(t *testing.T) {
	body := "Hallo mit Anhang — Grüße"
	data := []byte("PDF-BYTES-äöü-\x00\x01")
	raw, _ := Build(BuildOptions{
		From:    "a@b.test",
		To:      []string{"c@d.test"},
		Subject: "s",
		Body:    body,
		Domain:  "b.test",
		Attachments: []Attachment{
			{Filename: "Rechnung.pdf", ContentType: "application/pdf", Data: data},
		},
	})
	if !strings.Contains(string(raw), "multipart/mixed") {
		t.Fatalf("expected multipart/mixed, got: %s", string(raw)[:min(200, len(raw))])
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(p.Text, "Grüße") {
		t.Errorf("body text = %q", p.Text)
	}
	if len(p.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(p.Attachments))
	}
	a := p.Attachments[0]
	if a.Filename != "Rechnung.pdf" {
		t.Errorf("filename = %q", a.Filename)
	}
	if a.ContentType != "application/pdf" {
		t.Errorf("content-type = %q", a.ContentType)
	}
	if string(a.Data) != string(data) {
		t.Errorf("data round-trip mismatch: got %q want %q", a.Data, data)
	}
}

// TestHTMLAlternative: a rich-text body (no attachments) becomes multipart/alternative carrying
// both a text/plain and a text/html part, and Parse recovers both.
func TestHTMLAlternative(t *testing.T) {
	raw, _ := Build(BuildOptions{
		From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test",
		Body: "Hallo Welt", HTMLBody: "<p>Hallo <b>Welt</b></p>",
	})
	s := string(raw)
	if !strings.Contains(s, "multipart/alternative") || !strings.Contains(s, "text/html") {
		t.Fatalf("expected multipart/alternative with text/html:\n%s", s[:min(400, len(s))])
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(p.HTML, "<b>Welt</b>") {
		t.Errorf("html body lost: %q", p.HTML)
	}
	if !strings.Contains(p.Text, "Hallo Welt") {
		t.Errorf("text alternative lost: %q", p.Text)
	}
}

// TestHTMLWithAttachment: HTML body AND an attachment nests multipart/alternative inside
// multipart/mixed, and Parse recovers the html, the text, and the attachment bytes.
func TestHTMLWithAttachment(t *testing.T) {
	data := []byte("PDF-äöü")
	raw, _ := Build(BuildOptions{
		From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test",
		Body: "plain", HTMLBody: "<p>rich</p>",
		Attachments: []Attachment{{Filename: "f.pdf", ContentType: "application/pdf", Data: data}},
	})
	s := string(raw)
	if !strings.Contains(s, "multipart/mixed") || !strings.Contains(s, "multipart/alternative") {
		t.Fatalf("expected mixed wrapping alternative:\n%s", s[:min(400, len(s))])
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(p.HTML, "rich") || !strings.Contains(p.Text, "plain") {
		t.Errorf("bodies lost: text=%q html=%q", p.Text, p.HTML)
	}
	if len(p.Attachments) != 1 || string(p.Attachments[0].Data) != string(data) {
		t.Fatalf("attachment not recovered: %+v", p.Attachments)
	}
}

// TestHTMLSanitizeOutgoing: an outgoing HTML body with a script and an inline handler is stripped
// before it ever hits the wire, while ordinary formatting survives.
func TestHTMLSanitizeOutgoing(t *testing.T) {
	raw, _ := Build(BuildOptions{
		From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test",
		HTMLBody: `<p onclick="steal()">hi</p><script>evil()</script><b>keep</b>`,
	})
	p, _ := Parse(raw)
	low := strings.ToLower(p.HTML)
	if strings.Contains(low, "<script") || strings.Contains(low, "evil()") || strings.Contains(low, "onclick") {
		t.Errorf("dangerous content survived sanitisation: %q", p.HTML)
	}
	if !strings.Contains(p.HTML, "<b>keep</b>") {
		t.Errorf("formatting was stripped: %q", p.HTML)
	}
}

// TestHTMLTextFallback: when only an HTML body is given, Build derives a readable plain-text
// alternative from it (block tags → newlines, tags stripped, entities unescaped).
func TestHTMLTextFallback(t *testing.T) {
	raw, _ := Build(BuildOptions{
		From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test",
		HTMLBody: "<p>line&nbsp;one</p><p>line two &amp; more</p>",
	})
	p, _ := Parse(raw)
	if !strings.Contains(p.Text, "line one") || !strings.Contains(p.Text, "line two & more") {
		t.Errorf("derived text fallback wrong: %q", p.Text)
	}
}

// TestHTMLSanitizeBypasses pins the regression fixes for the adversarial-review findings: a
// slash-separated handler, a reconstituted script via an interposed tag, and obfuscated URL
// schemes must all be neutralised in the outgoing HTML.
func TestHTMLSanitizeBypasses(t *testing.T) {
	cases := []struct {
		name, in string
		banned   []string
	}{
		{"slash-handler", `<img/onerror=alert(1) src=x>`, []string{"onerror"}},
		{"quote-adjacent-handler", `<img src="x"onerror="alert(1)">`, []string{"onerror"}},
		{"reconstituted-script", `<scr<iframe>ipt>alert(1)</scr<iframe>ipt>keep`, []string{"<script", "</script", "alert(1)"}},
		{"entity-js-url", `<a href="java&#115;cript:alert(1)">x</a>`, []string{"javascript:", "java&#115;cript:"}},
		{"tab-js-url", "<a href=\"java\tscript:alert(1)\">x</a>", []string{"javascript:"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, _ := Build(BuildOptions{From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test", HTMLBody: c.in})
			p, _ := Parse(raw)
			low := strings.ToLower(p.HTML)
			for _, b := range c.banned {
				if strings.Contains(low, strings.ToLower(b)) {
					t.Errorf("banned %q survived sanitisation: html=%q", b, p.HTML)
				}
			}
		})
	}
	// A safe link and formatting must be preserved.
	raw, _ := Build(BuildOptions{From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test", HTMLBody: `<p>hi <b>x</b> <a href="https://ok.test">link</a></p>`})
	p, _ := Parse(raw)
	if !strings.Contains(p.HTML, "<b>x</b>") || !strings.Contains(p.HTML, "https://ok.test") {
		t.Errorf("benign content was stripped: %q", p.HTML)
	}
}

func TestCalendarRoundTrip(t *testing.T) {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//icaly//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:evt-1\r\nDTSTART:20260901T090000Z\r\nSUMMARY:Sync\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw, _ := Build(BuildOptions{
		From:           "alice@b.test",
		To:             []string{"ext@gmail.com"},
		Subject:        "Invitation: Sync",
		Body:           "You are invited.",
		Domain:         "b.test",
		CalendarICS:    ics,
		CalendarMethod: "REQUEST",
	})
	s := string(raw)
	if !strings.Contains(s, "multipart/alternative") || !strings.Contains(s, "text/calendar") || !strings.Contains(s, "method=REQUEST") {
		t.Fatalf("expected iMIP multipart/alternative:\n%s", s[:min(400, len(s))])
	}
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(p.Calendar, "UID:evt-1") {
		t.Errorf("calendar part not captured: %q", p.Calendar)
	}
	if p.CalendarMethod != "REQUEST" {
		t.Errorf("calendar method = %q, want REQUEST", p.CalendarMethod)
	}
	if !strings.Contains(p.Text, "You are invited.") {
		t.Errorf("text body lost: %q", p.Text)
	}
}

func TestCalendarInBodyMethodWins(t *testing.T) {
	// MIME says REQUEST but the body says REPLY — the in-body METHOD must win (plan m4).
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:e\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw, _ := Build(BuildOptions{From: "a@b.test", To: []string{"c@d.test"}, Subject: "s", Domain: "b.test", CalendarICS: ics, CalendarMethod: "REQUEST"})
	p, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.CalendarMethod != "REPLY" {
		t.Errorf("expected in-body REPLY to win, got %q", p.CalendarMethod)
	}
}

// TestNoHeaderInjection is the regression for the CRLF header-injection finding: CR/LF smuggled
// into Subject or a recipient must never start a new header line in the built message.
func TestNoHeaderInjection(t *testing.T) {
	raw, _ := Build(BuildOptions{
		From:    "a@b.test",
		To:      []string{"c@d.test\r\nBcc: victim@evil.test"},
		Subject: "Hi\r\nBcc: victim2@evil.test\r\nX-Evil: 1",
		Body:    "body",
		Domain:  "b.test",
	})
	head := string(raw)
	if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
		head = head[:i]
	}
	for _, ln := range strings.Split(head, "\r\n") {
		low := strings.ToLower(strings.TrimSpace(ln))
		if strings.HasPrefix(low, "bcc:") || strings.HasPrefix(low, "x-evil:") {
			t.Errorf("injected header survived: %q", ln)
		}
	}
}

func firstHeader(raw, key string) string {
	for _, ln := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(ln, key+":") {
			return ln
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
