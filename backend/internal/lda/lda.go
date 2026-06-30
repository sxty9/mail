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
	"strings"

	"mail/internal/maildir"
	"mail/internal/message"
	"mail/internal/outbound"
	"mail/internal/profile"
	"mail/internal/registry"
)

// Deliverer wires the store, outbound spool, profile resolver and identity registry together.
// The registry is the authority on whether an address is local and whose mailbox it is.
type Deliverer struct {
	store *maildir.Store
	out   *outbound.Queue
	prof  *profile.Resolver
	reg   *registry.Registry
}

// New builds a Deliverer.
func New(store *maildir.Store, out *outbound.Queue, prof *profile.Resolver, reg *registry.Registry) *Deliverer {
	return &Deliverer{store: store, out: out, prof: prof, reg: reg}
}

// SendInput is a compose request from the UI/JMAP layer.
type SendInput struct {
	FromUser    string // the authenticated local user
	FromAddr    string // optional send-as address; must be one the user owns (else ignored)
	To          []string
	Cc          []string
	Subject     string
	Body        string
	HTMLBody    string // optional rich-text HTML body (sanitised in message.Build)
	InReplyTo   string
	References   []string
	Attachments []message.Attachment
	// CalendarICS/CalendarMethod, when set, send an iMIP calendar message (used by icaly).
	CalendarICS    string
	CalendarMethod string
}

// SendResult summarises what happened to a sent message.
type SendResult struct {
	MessageID       string `json:"messageId"`
	DeliveredLocal  int    `json:"deliveredLocal"`
	QueuedExternal  int    `json:"queuedExternal"`
	EdgeConfigured  bool   `json:"edgeConfigured"`
}

// Send composes, stores in Sent, delivers locally and spools externally. The From address is
// the user's default unless they requested a send-as address they actually own; the Message-ID
// is keyed to the From address's domain so it stays aligned with whatever domain we send as.
func (d *Deliverer) Send(in SendInput) (SendResult, error) {
	from := d.reg.DefaultAddress(in.FromUser)
	if in.FromAddr != "" {
		// Reject — never silently send as the default when the caller asked for an address the
		// user does not own. This keeps the send-as contract authoritative at the LDA, not just
		// at the HTTP layer.
		if !d.reg.Owns(in.FromUser, in.FromAddr) {
			return SendResult{}, errors.New("send-as address not owned by user")
		}
		from = strings.ToLower(strings.TrimSpace(in.FromAddr))
	}
	raw, msgID := message.Build(message.BuildOptions{
		From:        from,
		FromName:    d.prof.Load(in.FromUser).DisplayName(),
		To:          in.To,
		Cc:          in.Cc,
		Subject:     in.Subject,
		Body:        in.Body,
		HTMLBody:       in.HTMLBody,
		InReplyTo:      in.InReplyTo,
		References:     in.References,
		Domain:         domainOf(from),
		Attachments:    in.Attachments,
		CalendarICS:    in.CalendarICS,
		CalendarMethod: in.CalendarMethod,
	})

	res := SendResult{MessageID: msgID, EdgeConfigured: d.out.EdgeConfigured()}

	// Classify recipients up front with no side effects, then hand external mail to the spool BEFORE
	// writing any local copy. If the spool write fails we abort with nothing delivered, so a retry
	// cannot duplicate the Sent copy or the local deliveries (the bug a deliver-then-enqueue order
	// had: a queue failure surfaced as "send failed" after mail was already delivered locally).
	var localUsers []string
	var external []string
	seenLocal := map[string]bool{}
	for _, addr := range append(append([]string{}, in.To...), in.Cc...) {
		if u, ok := d.localUser(addr); ok {
			if !seenLocal[u] { // one copy per mailbox even if addressed via several aliases
				seenLocal[u] = true
				localUsers = append(localUsers, u)
			}
		} else if a := parseAddr(addr); a != "" {
			external = append(external, a)
		}
	}
	if len(external) > 0 {
		if _, err := d.out.Enqueue(from, external, raw); err != nil {
			return SendResult{}, err
		}
		res.QueuedExternal = len(external)
	}

	// Sender's own copy (already Seen), then each local recipient's INBOX.
	if _, err := d.store.Deliver(in.FromUser, "Sent", raw, true); err != nil {
		return res, err
	}
	for _, u := range localUsers {
		if _, err := d.store.Deliver(u, "INBOX", raw, false); err == nil {
			res.DeliveredLocal++
		}
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

// localUser reports whether addr belongs to a local holistic user on this instance. The registry
// decides which mailbox (alias or default <user>@<served domain>); we then confirm that mailbox's
// owner is a real OS account before delivering.
func (d *Deliverer) localUser(addr string) (string, bool) {
	a := parseAddr(addr)
	if a == "" {
		return "", false
	}
	// Defense in depth: a real local account always owns its own canonical address
	// <account>@<served domain>; no alias may redirect it to another mailbox. This holds even
	// for an alias registered before that account existed.
	if at := strings.LastIndex(a, "@"); at > 0 {
		localpart, domain := strings.ToLower(a[:at]), a[at+1:]
		if d.reg.Serves(domain) {
			if _, err := user.Lookup(localpart); err == nil {
				return localpart, true
			}
		}
	}
	u, ok := d.reg.Resolve(a)
	if !ok {
		return "", false
	}
	if _, err := user.Lookup(u); err != nil {
		return "", false
	}
	return u, true
}

// domainOf returns the domain part of an address, or "" if it has none.
func domainOf(addr string) string {
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		return addr[at+1:]
	}
	return ""
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
