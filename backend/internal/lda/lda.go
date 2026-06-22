// Package lda is maild's local delivery agent and the one place that decides a message's
// fate: compose it, drop a copy in the sender's Sent, deliver to every LOCAL holistic user
// directly into their Maildir (no SMTP for internal mail), and spool anything addressed off
// this instance to the outbound queue (which the sxgate edge later relays). Inbound internet
// mail (handed in by the edge) flows through DeliverInbound into the recipients' INBOX.
package lda

import (
	"errors"
	"net/mail"
	"os/user"
	"regexp"
	"strings"

	"mail/internal/instance"
	"mail/internal/maildir"
	"mail/internal/message"
	"mail/internal/outbound"
)

var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Deliverer wires the store, outbound spool and domain resolver together.
type Deliverer struct {
	store *maildir.Store
	out   *outbound.Queue
	inst  *instance.Resolver
}

// New builds a Deliverer.
func New(store *maildir.Store, out *outbound.Queue, inst *instance.Resolver) *Deliverer {
	return &Deliverer{store: store, out: out, inst: inst}
}

// SendInput is a compose request from the UI/JMAP layer.
type SendInput struct {
	FromUser   string // the authenticated local user
	To         []string
	Cc         []string
	Subject    string
	Body       string
	InReplyTo  string
	References []string
}

// SendResult summarises what happened to a sent message.
type SendResult struct {
	MessageID       string `json:"messageId"`
	DeliveredLocal  int    `json:"deliveredLocal"`
	QueuedExternal  int    `json:"queuedExternal"`
	EdgeConfigured  bool   `json:"edgeConfigured"`
}

// Send composes, stores in Sent, delivers locally and spools externally.
func (d *Deliverer) Send(in SendInput) (SendResult, error) {
	from := d.inst.Address(in.FromUser)
	raw, msgID := message.Build(message.BuildOptions{
		From:       from,
		To:         in.To,
		Cc:         in.Cc,
		Subject:    in.Subject,
		Body:       in.Body,
		InReplyTo:  in.InReplyTo,
		References: in.References,
		Domain:     d.inst.MailDomain(),
	})

	// Always keep the sender's own copy (already Seen).
	if _, err := d.store.Deliver(in.FromUser, "Sent", raw, true); err != nil {
		return SendResult{}, err
	}

	res := SendResult{MessageID: msgID, EdgeConfigured: d.out.EdgeConfigured()}
	var external []string
	for _, addr := range append(append([]string{}, in.To...), in.Cc...) {
		if u, ok := d.localUser(addr); ok {
			if _, err := d.store.Deliver(u, "INBOX", raw, false); err == nil {
				res.DeliveredLocal++
			}
		} else if a := parseAddr(addr); a != "" {
			external = append(external, a)
		}
	}
	if len(external) > 0 {
		if _, err := d.out.Enqueue(from, external, raw); err != nil {
			return res, err
		}
		res.QueuedExternal = len(external)
	}
	return res, nil
}

// DeliverInbound writes an inbound internet message (already raw RFC 5322, handed in by the
// sxgate edge) into each local recipient's INBOX. Returns how many local mailboxes received
// it; an error if none did.
func (d *Deliverer) DeliverInbound(rcpts []string, raw []byte) (int, error) {
	delivered := 0
	for _, addr := range rcpts {
		if u, ok := d.localUser(addr); ok {
			if _, err := d.store.Deliver(u, "INBOX", raw, false); err == nil {
				delivered++
			}
		}
	}
	if delivered == 0 {
		return 0, errors.New("no local recipient")
	}
	return delivered, nil
}

// localUser reports whether addr belongs to a local holistic user on this instance.
func (d *Deliverer) localUser(addr string) (string, bool) {
	a := parseAddr(addr)
	if a == "" {
		return "", false
	}
	at := strings.LastIndex(a, "@")
	if at < 0 {
		return "", false
	}
	localpart, domain := strings.ToLower(a[:at]), strings.ToLower(a[at+1:])
	if md := d.inst.MailDomain(); md != "" && !strings.EqualFold(domain, md) {
		return "", false // addressed off this instance
	}
	if !usernameRE.MatchString(localpart) {
		return "", false
	}
	if _, err := user.Lookup(localpart); err != nil {
		return "", false
	}
	return localpart, true
}

// parseAddr normalises "Name <a@b>" or "a@b" to the bare address, or "" if unparseable.
func parseAddr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Address
	}
	if strings.Contains(s, "@") && !strings.ContainsAny(s, " <>") {
		return s
	}
	return ""
}
