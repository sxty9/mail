// Package api serves maild's HTTP surface under /api/services/mail/, behind the shared
// holistic session. Reads/manage are gated by hp_mail_read, sending by hp_mail_send, both
// scoped to the caller's OWN mailbox. The inbound webhook is the one machine-to-machine
// endpoint: it is authenticated by a shared secret (the sxgate mail edge calls it), not the
// user JWT. Error bodies follow holistic's contract: {"detail": "..."}.
package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"mail/internal/apppass"
	"mail/internal/auth"
	"mail/internal/instance"
	"mail/internal/jmap"
	"mail/internal/lda"
	"mail/internal/maildir"
	"mail/internal/message"
	"mail/internal/registry"
	"mail/internal/rights"
)

const (
	base    = "/api/services/mail/"
	service = "mail"
	version = "0.1.0"

	maxComposeBytes = 30 << 20 // 30 MiB compose payload
	maxInboundBytes = 50 << 20 // 50 MiB inbound message
)

// Server wires the verifier, store, registry and delivery agent into HTTP handlers.
type Server struct {
	v             *auth.Verifier
	store         *maildir.Store
	inst          *instance.Resolver
	reg           *registry.Registry
	lda           *lda.Deliverer
	apppass       *apppass.Store
	jmap          *jmap.Server
	inboundSecret string
	// Calendar (icaly) integration — all optional; empty disables it and maild behaves exactly
	// as before. internalSecret guards POST internal/send (icaly sends iMIP on a user's behalf);
	// calURL/calSecret are how maild forwards inbound text/calendar messages to icaly.
	internalSecret string
	calURL         string
	calSecret      string
}

// New builds a server. calURL/calSecret/internalSecret wire the optional icaly calendar
// integration; pass "" to disable it (maild then behaves identically to before).
func New(v *auth.Verifier, store *maildir.Store, inst *instance.Resolver, reg *registry.Registry, del *lda.Deliverer, ap *apppass.Store, inboundSecret, internalSecret, calURL, calSecret string) *Server {
	return &Server{
		v: v, store: store, inst: inst, reg: reg, lda: del, apppass: ap,
		jmap: jmap.New(store, inst), inboundSecret: inboundSecret,
		internalSecret: internalSecret, calURL: calURL, calSecret: calSecret,
	}
}

type handler func(w http.ResponseWriter, r *http.Request, u *auth.User)

// Handler returns the routed http.Handler (Go 1.22 method+path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET "+base+"info", s.guard("", false, s.info))

	// Mailbox reads/management (own mailbox), gated by hp_mail_read.
	mux.HandleFunc("GET "+base+"mailboxes", s.guard(rights.GroupRead, false, s.mailboxes))
	mux.HandleFunc("GET "+base+"messages", s.guard(rights.GroupRead, false, s.messages))
	mux.HandleFunc("GET "+base+"message", s.guard(rights.GroupRead, false, s.message))
	mux.HandleFunc("GET "+base+"attachment", s.guard(rights.GroupRead, false, s.attachment))
	mux.HandleFunc("POST "+base+"flags", s.guard(rights.GroupRead, true, s.flags))
	mux.HandleFunc("POST "+base+"move", s.guard(rights.GroupRead, true, s.move))
	mux.HandleFunc("POST "+base+"delete", s.guard(rights.GroupRead, true, s.delete))

	// Custom folder management (own mailbox), gated by hp_mail_read. Create/rename accept nested
	// names ("Work/Projects"); reorder persists the user's chosen display order.
	mux.HandleFunc("POST "+base+"folders/create", s.guard(rights.GroupRead, true, s.folderCreate))
	mux.HandleFunc("POST "+base+"folders/rename", s.guard(rights.GroupRead, true, s.folderRename))
	mux.HandleFunc("POST "+base+"folders/delete", s.guard(rights.GroupRead, true, s.folderDelete))
	mux.HandleFunc("POST "+base+"folders/reorder", s.guard(rights.GroupRead, true, s.folderReorder))

	// Sending, gated by hp_mail_send.
	mux.HandleFunc("POST "+base+"send", s.guard(rights.GroupSend, true, s.send))
	// Drafts: save (compose right) and discard (own mailbox).
	mux.HandleFunc("POST "+base+"drafts/save", s.guard(rights.GroupSend, true, s.draftSave))
	mux.HandleFunc("POST "+base+"drafts/delete", s.guard(rights.GroupRead, true, s.draftDelete))

	// Administration of domains + aliases (provisioning only — never reads a mailbox), gated by
	// hp_mail_admin. This right is orthogonal to hp_mail_read: an admin manages addressing, not
	// other people's mail.
	mux.HandleFunc("GET "+base+"admin/domains", s.guard(rights.GroupAdmin, false, s.adminDomains))
	mux.HandleFunc("POST "+base+"admin/domains", s.guard(rights.GroupAdmin, true, s.adminAddDomain))
	mux.HandleFunc("POST "+base+"admin/domains/delete", s.guard(rights.GroupAdmin, true, s.adminDeleteDomain))
	mux.HandleFunc("GET "+base+"admin/aliases", s.guard(rights.GroupAdmin, false, s.adminAliases))
	mux.HandleFunc("POST "+base+"admin/aliases", s.guard(rights.GroupAdmin, true, s.adminAddAlias))
	mux.HandleFunc("POST "+base+"admin/aliases/delete", s.guard(rights.GroupAdmin, true, s.adminDeleteAlias))
	mux.HandleFunc("GET "+base+"admin/user", s.guard(rights.GroupAdmin, false, s.adminUser))

	// App passwords for native clients — managed from the browser session (CSRF-protected).
	mux.HandleFunc("GET "+base+"apppasswords", s.guard(rights.GroupRead, false, s.appList))
	mux.HandleFunc("POST "+base+"apppasswords", s.guard(rights.GroupRead, true, s.appCreate))
	mux.HandleFunc("POST "+base+"apppasswords/delete", s.guard(rights.GroupRead, true, s.appDelete))

	// JMAP — usable by the holistic session OR a native client with an app password (Basic auth).
	mux.HandleFunc("GET "+base+"jmap/session", s.guardMail(rights.GroupRead, s.jmapSession))
	mux.HandleFunc("POST "+base+"jmap/api", s.guardMail(rights.GroupRead, s.jmapAPI))

	// Inbound edge webhook — secret-authenticated, NOT the user session.
	mux.HandleFunc("POST "+base+"inbound", s.inbound)

	// Daemon-to-daemon send on a user's behalf (icaly iMIP egress) — shared-secret auth, never
	// a session and never hp_mail_send inside maild (icaly is the trust boundary that gates it).
	mux.HandleFunc("POST "+base+"internal/send", s.internalSend)
	return mux
}

// guard authenticates, optionally requires a fine-grained right, and optionally enforces CSRF.
func (s *Server) guard(perm string, csrf bool, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.v.User(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "Not authenticated")
			return
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission for this action")
			return
		}
		if csrf && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		h(w, r, u)
	}
}

// guardMail authenticates via the holistic session OR a maild app password (HTTP Basic), then
// enforces the right. CSRF is required only on the session path (cookie auth); app-password
// clients carry no cookie and are not CSRF-exposed.
func (s *Server) guardMail(perm string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.v.User(r)
		sessionAuthed := err == nil
		if !sessionAuthed {
			u, err = s.v.AppUser(r, s.apppass)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Basic realm="maild", charset="UTF-8"`)
				writeErr(w, http.StatusUnauthorized, "Not authenticated")
				return
			}
		}
		if perm != "" && !u.Can(perm) {
			writeErr(w, http.StatusForbidden, "You do not have permission for this action")
			return
		}
		if sessionAuthed && r.Method == http.MethodPost && !s.v.CheckCSRF(r) {
			writeErr(w, http.StatusForbidden, "CSRF check failed")
			return
		}
		h(w, r, u)
	}
}

func (s *Server) jmapSession(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, s.jmap.Session(u.Username))
}

func (s *Server) jmapAPI(w http.ResponseWriter, r *http.Request, u *auth.User) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxComposeBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Could not read request")
		return
	}
	resp, status := s.jmap.API(u.Username, body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(resp)
}

func (s *Server) appList(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	list, err := s.apppass.List(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not read app passwords")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": u.Username, "appPasswords": list})
}

func (s *Server) appCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		Label string `json:"label"`
	}
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(req.Label) == "" {
		req.Label = "Mail app"
	}
	token, meta, err := s.apppass.Create(u.Username, req.Label)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not create app password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       meta.ID,
		"label":    meta.Label,
		"created":  meta.Created,
		"token":    token, // shown once
		"username": u.Username,
		"jmapUrl":  "/api/services/mail/jmap/session",
	})
}

func (s *Server) appDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, 4096, &req) || strings.TrimSpace(req.ID) == "" {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := s.apppass.Delete(u.Username, req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not delete app password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":    service,
		"version":    version,
		"user":       u.Username,
		"isAdmin":    u.IsAdmin,
		"address":    s.reg.DefaultAddress(u.Username),
		"addresses":  s.reg.Addresses(u.Username), // every address the user may send from
		"mailDomain": s.inst.MailDomain(),
		"canAdmin":   u.Can(rights.GroupAdmin),
		"canSend":    u.Can(rights.GroupSend),
	})
}

func (s *Server) mailboxes(w http.ResponseWriter, _ *http.Request, u *auth.User) {
	folders, err := s.store.Folders(u.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not open mailbox")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"address":    s.inst.Address(u.Username),
		"mailDomain": s.inst.MailDomain(),
		"folders":    folders,
	})
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, u *auth.User) {
	folder := folderParam(r)
	if !maildir.ValidFolder(folder) || !s.store.HasFolder(u.Username, folder) {
		writeErr(w, http.StatusNotFound, "Unknown mailbox")
		return
	}
	msgs, err := s.store.List(u.Username, folder)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not list messages")
		return
	}
	views := make([]msgView, 0, len(msgs))
	for _, m := range msgs {
		views = append(views, toView(m))
	}
	writeJSON(w, http.StatusOK, map[string]any{"folder": folder, "messages": views})
}

func (s *Server) message(w http.ResponseWriter, r *http.Request, u *auth.User) {
	folder := folderParam(r)
	if !maildir.ValidFolder(folder) {
		writeErr(w, http.StatusBadRequest, "Unknown mailbox")
		return
	}
	id, ok := decodeID(r.URL.Query().Get("id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "Invalid message id")
		return
	}
	raw, err := s.store.ReadRaw(u.Username, folder, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Message not found")
		return
	}
	p, err := message.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not parse message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          r.URL.Query().Get("id"),
		"folder":      folder,
		"from":        p.From,
		"to":          p.To,
		"cc":          p.Cc,
		"bcc":         p.Bcc,
		"subject":     p.Subject,
		"date":        rfc3339(p.Date),
		"messageId":   p.MessageID,
		"inReplyTo":   p.InReplyTo,
		"references":  p.References,
		"text":        p.Text,
		"html":        p.HTML,
		"attachments": attachmentViews(p.Attachments),
	})
}

// attachment streams one decoded attachment (by its index in the parsed message) as a download.
func (s *Server) attachment(w http.ResponseWriter, r *http.Request, u *auth.User) {
	folder := folderParam(r)
	if !maildir.ValidFolder(folder) {
		writeErr(w, http.StatusBadRequest, "Unknown mailbox")
		return
	}
	id, ok := decodeID(r.URL.Query().Get("id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "Invalid message id")
		return
	}
	idx, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil || idx < 0 {
		writeErr(w, http.StatusBadRequest, "Invalid attachment index")
		return
	}
	raw, err := s.store.ReadRaw(u.Username, folder, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "Message not found")
		return
	}
	p, err := message.Parse(raw)
	if err != nil || idx >= len(p.Attachments) {
		writeErr(w, http.StatusNotFound, "Attachment not found")
		return
	}
	a := p.Attachments[idx]
	ct := a.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Inline display is opt-in (?inline=1) AND only for types that cannot execute script in our
	// origin — images (not SVG), PDF, audio/video, plain text. Everything else is force-downloaded.
	// A CSP sandbox hardens the one case (PDF via <object>) that becomes a nested browsing context.
	if r.URL.Query().Get("inline") == "1" && inlineSafe(ct) {
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self' data:; media-src 'self' data:; object-src 'self'; style-src 'unsafe-inline'; sandbox")
	} else {
		w.Header().Set("Content-Disposition", formatAttachmentDisposition(a.Filename, idx))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Data)
}

type attachmentIn struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Data        string `json:"data"` // base64-encoded bytes
}

type sendReq struct {
	From        string          `json:"from"` // optional send-as address; must be one the user owns
	To          []string        `json:"to"`
	Cc          []string        `json:"cc"`
	Bcc         []string        `json:"bcc"` // blind copies — delivered but never written to headers
	Subject     string          `json:"subject"`
	Body        string          `json:"body"`
	HTMLBody    string          `json:"htmlBody"` // optional rich-text HTML (sanitised in message.Build)
	InReplyTo   string          `json:"inReplyTo"`
	References  []string        `json:"references"`
	Attachments []attachmentIn  `json:"attachments"`
	ReplaceID   string          `json:"replaceId"` // drafts/save only: previous draft id to remove
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req sendReq
	if !decodeBody(w, r, maxComposeBytes, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(cleanAddrs(req.To))+len(cleanAddrs(req.Cc))+len(cleanAddrs(req.Bcc)) == 0 {
		writeErr(w, http.StatusBadRequest, "At least one recipient is required")
		return
	}
	// A requested send-as address must be one the user actually owns; reject rather than
	// silently fall back, so the user never thinks they sent as an address they didn't.
	from := strings.TrimSpace(req.From)
	if from != "" && !s.reg.Owns(u.Username, from) {
		writeErr(w, http.StatusForbidden, "You may not send from that address")
		return
	}
	atts, err := decodeAttachments(req.Attachments)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid attachment data")
		return
	}
	res, err := s.lda.Send(lda.SendInput{
		FromUser:    u.Username,
		FromAddr:    from,
		To:          cleanAddrs(req.To),
		Cc:          cleanAddrs(req.Cc),
		Bcc:         cleanAddrs(req.Bcc),
		Subject:     req.Subject,
		Body:        req.Body,
		HTMLBody:    req.HTMLBody,
		InReplyTo:   strings.TrimSpace(req.InReplyTo),
		References:  req.References,
		Attachments: atts,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not send message")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// draftSave builds the compose payload exactly like send, but stores it in Drafts only (no
// delivery). replaceId, when set, is the previous draft version to remove.
func (s *Server) draftSave(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req sendReq
	if !decodeBody(w, r, maxComposeBytes, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	from := strings.TrimSpace(req.From)
	if from != "" && !s.reg.Owns(u.Username, from) {
		writeErr(w, http.StatusForbidden, "You may not send from that address")
		return
	}
	atts, err := decodeAttachments(req.Attachments)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid attachment data")
		return
	}
	replace := ""
	if req.ReplaceID != "" {
		if id, ok := decodeID(req.ReplaceID); ok {
			replace = id
		}
	}
	id, err := s.lda.SaveDraft(lda.SendInput{
		FromUser:    u.Username,
		FromAddr:    from,
		To:          cleanAddrs(req.To),
		Cc:          cleanAddrs(req.Cc),
		Bcc:         cleanAddrs(req.Bcc),
		Subject:     req.Subject,
		Body:        req.Body,
		HTMLBody:    req.HTMLBody,
		InReplyTo:   strings.TrimSpace(req.InReplyTo),
		References:  req.References,
		Attachments: atts,
	}, replace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not save draft")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": encodeID(id)})
}

func (s *Server) draftDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	id, ok := decodeID(req.ID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Invalid draft id")
		return
	}
	if err := s.store.Remove(u.Username, "Drafts", id); err != nil {
		writeErr(w, http.StatusNotFound, "Draft not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type flagsReq struct {
	Mailbox  string `json:"mailbox"`
	ID       string `json:"id"`
	Seen     *bool  `json:"seen"`
	Flagged  *bool  `json:"flagged"`
	Answered *bool  `json:"answered"`
}

func (s *Server) flags(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req flagsReq
	if !decodeBody(w, r, 4096, &req) || !maildir.ValidFolder(req.Mailbox) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	id, ok := decodeID(req.ID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Invalid message id")
		return
	}
	if err := s.store.SetFlags(u.Username, req.Mailbox, id, maildir.Flags{Seen: req.Seen, Flagged: req.Flagged, Answered: req.Answered}); err != nil {
		writeErr(w, http.StatusNotFound, "Message not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type moveReq struct {
	Mailbox string `json:"mailbox"`
	ID      string `json:"id"`
	To      string `json:"to"`
}

func (s *Server) move(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req moveReq
	if !decodeBody(w, r, 4096, &req) || !maildir.ValidFolder(req.Mailbox) || !maildir.ValidFolder(req.To) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	// The destination must be a real mailbox — never let a move conjure a folder into existence.
	if !s.store.HasFolder(u.Username, req.To) {
		writeErr(w, http.StatusBadRequest, "Unknown destination mailbox")
		return
	}
	id, ok := decodeID(req.ID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Invalid message id")
		return
	}
	if err := s.store.Move(u.Username, req.Mailbox, req.To, id); err != nil {
		writeErr(w, http.StatusNotFound, "Message not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type deleteReq struct {
	Mailbox string `json:"mailbox"`
	ID      string `json:"id"`
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req deleteReq
	if !decodeBody(w, r, 4096, &req) || !maildir.ValidFolder(req.Mailbox) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	id, ok := decodeID(req.ID)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Invalid message id")
		return
	}
	if err := s.store.Delete(u.Username, req.Mailbox, id); err != nil {
		writeErr(w, http.StatusNotFound, "Message not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── custom folders ───────────────────────────────────────────────────────────────────

type folderReq struct {
	Name    string `json:"name"`
	NewName string `json:"newName"`
}

func (s *Server) folderCreate(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req folderReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := s.store.CreateFolder(u.Username, req.Name); err != nil {
		writeErr(w, folderErrStatus(err), folderErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) folderRename(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req folderReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := s.store.RenameFolder(u.Username, req.Name, req.NewName); err != nil {
		writeErr(w, folderErrStatus(err), folderErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) folderDelete(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req folderReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := s.store.DeleteFolder(u.Username, req.Name); err != nil {
		writeErr(w, folderErrStatus(err), folderErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type reorderReq struct {
	Order []string `json:"order"` // custom folder names in the desired display order
}

func (s *Server) folderReorder(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req reorderReq
	// req.Order must be present: an empty/absent body decodes to nil, and persisting nil would wipe
	// the saved display order back to alphabetical. Require an explicit (possibly empty) array.
	if !decodeBody(w, r, 64<<10, &req) || req.Order == nil {
		writeErr(w, http.StatusBadRequest, "A folder order is required")
		return
	}
	if err := s.store.SetFolderOrder(u.Username, req.Order); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not reorder folders")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func folderErrStatus(err error) int {
	switch {
	case errors.Is(err, maildir.ErrInvalidFolder):
		return http.StatusBadRequest
	case errors.Is(err, maildir.ErrFolderExists):
		return http.StatusConflict
	case errors.Is(err, os.ErrNotExist):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func folderErrMsg(err error) string {
	switch {
	case errors.Is(err, maildir.ErrInvalidFolder):
		return "Invalid folder name"
	case errors.Is(err, maildir.ErrFolderExists):
		return "A folder with that name already exists"
	case errors.Is(err, os.ErrNotExist):
		return "Folder not found"
	default:
		return "Could not update folder"
	}
}

// ── admin: domains + aliases (provisioning only, never reads a mailbox) ───────────────

func (s *Server) adminDomains(w http.ResponseWriter, _ *http.Request, _ *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{"domains": s.reg.Domains()})
}

type domainReq struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

func (s *Server) adminAddDomain(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	var req domainReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	d, err := s.reg.AddDomain(req.Name, req.Provider)
	if err != nil {
		writeErr(w, registryErrStatus(err), registryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": d})
}

func (s *Server) adminDeleteDomain(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	var req domainReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := s.reg.RemoveDomain(req.Name); err != nil {
		writeErr(w, registryErrStatus(err), registryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) adminAliases(w http.ResponseWriter, _ *http.Request, _ *auth.User) {
	writeJSON(w, http.StatusOK, map[string]any{"aliases": s.reg.Aliases()})
}

type aliasReq struct {
	Address string `json:"address"`
	User    string `json:"user"`
}

func (s *Server) adminAddAlias(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	var req aliasReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	// The alias target must be a real local account — never point an address at a phantom user.
	if !userExists(req.User) {
		writeErr(w, http.StatusBadRequest, "Unknown target user")
		return
	}
	// An alias may never shadow another existing user's own canonical address (e.g. routing
	// alice@domain to mallory). If the localpart is itself a real account, the only legal target
	// is that same account. This keeps hp_mail_admin from redirecting a user's inbound mail.
	if lp := localpartOf(req.Address); userExists(lp) && !strings.EqualFold(lp, req.User) {
		writeErr(w, http.StatusConflict, "That address already belongs to another user")
		return
	}
	a, err := s.reg.AddAlias(req.Address, req.User)
	if err != nil {
		writeErr(w, registryErrStatus(err), registryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alias": a})
}

// localpartOf returns the lowercased localpart of an address, or "" if it has no "@".
func localpartOf(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if at := strings.LastIndex(addr, "@"); at > 0 {
		return addr[:at]
	}
	return ""
}

func (s *Server) adminDeleteAlias(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	var req aliasReq
	if !decodeBody(w, r, 4096, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request")
		return
	}
	if err := s.reg.RemoveAlias(req.Address); err != nil {
		writeErr(w, registryErrStatus(err), registryErrMsg(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// adminUser returns the addresses + aliases a given user owns, for the admin's per-user view. It
// deliberately touches only the registry, never the user's mailbox (admin ≠ read).
func (s *Server) adminUser(w http.ResponseWriter, r *http.Request, _ *auth.User) {
	target := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("user")))
	if !userExists(target) {
		writeErr(w, http.StatusNotFound, "Unknown user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":      target,
		"address":   s.reg.DefaultAddress(target),
		"addresses": s.reg.Addresses(target),
		"aliases":   s.reg.AliasesFor(target),
	})
}

func registryErrStatus(err error) int {
	switch {
	case errors.Is(err, registry.ErrInvalidDomain), errors.Is(err, registry.ErrInvalidAlias), errors.Is(err, registry.ErrInvalidUser):
		return http.StatusBadRequest
	case errors.Is(err, registry.ErrPrimaryDomain):
		return http.StatusForbidden
	case errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func registryErrMsg(err error) string {
	switch {
	case errors.Is(err, registry.ErrInvalidDomain):
		return "Invalid domain name"
	case errors.Is(err, registry.ErrInvalidAlias):
		return "Invalid alias address (must be a localpart on a served domain)"
	case errors.Is(err, registry.ErrInvalidUser):
		return "Invalid target user"
	case errors.Is(err, registry.ErrPrimaryDomain):
		return "The primary domain cannot be removed"
	case errors.Is(err, registry.ErrNotFound):
		return "Not found"
	default:
		return "Could not update the registry"
	}
}

// inbound is the sxgate mail edge → maild webhook. Authenticated by a shared secret (the
// edge holds it), NOT the user session. Body is a raw RFC 5322 message; recipients come from
// the X-Mail-Rcpt header (comma-separated) or X-Original-To.
func (s *Server) inbound(w http.ResponseWriter, r *http.Request) {
	if s.inboundSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "Inbound not configured")
		return
	}
	got := r.Header.Get("X-Mail-Inbound-Secret")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.inboundSecret)) != 1 {
		writeErr(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	rcpts := cleanAddrs(splitList(r.Header.Get("X-Mail-Rcpt")))
	if len(rcpts) == 0 {
		rcpts = cleanAddrs(splitList(r.Header.Get("X-Original-To")))
	}
	if len(rcpts) == 0 {
		writeErr(w, http.StatusBadRequest, "No recipient")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInboundBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "Could not read message")
		return
	}
	n, err := s.lda.DeliverInbound(rcpts, raw)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "No local recipient")
		return
	}
	// Hand any calendar-bearing message to icaly (it owns all iTIP logic); maild stays a dumb
	// pipe and does not parse METHOD. Fire-and-forget so delivery latency is unaffected.
	s.forwardCalendar(rcpts, r.Header.Get("X-Mail-From"), r.Header.Get("X-Message-Id"), raw)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "delivered": n})
}

// forwardCalendar POSTs a raw message that carries any text/calendar part to icaly's inbound
// iMIP webhook. No-op unless the calendar integration is configured. Best-effort, async.
func (s *Server) forwardCalendar(rcpts []string, from, messageID string, raw []byte) {
	if s.calURL == "" || s.calSecret == "" {
		return
	}
	p, err := message.Parse(raw)
	if err != nil || p.Calendar == "" {
		return
	}
	body := append([]byte(nil), raw...)
	go func() {
		req, err := http.NewRequest(http.MethodPost, s.calURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "message/rfc822")
		req.Header.Set("X-Cal-Inbound-Secret", s.calSecret)
		req.Header.Set("X-Mail-From", from)
		req.Header.Set("X-Mail-Rcpt", strings.Join(rcpts, ", "))
		req.Header.Set("X-Message-Id", messageID)
		client := &http.Client{Timeout: 10 * time.Second}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
}

type internalSendReq struct {
	From           string `json:"from"` // the local user to send on behalf of
	To             []string `json:"to"`
	Cc             []string `json:"cc"`
	Subject        string `json:"subject"`
	Body           string `json:"body"`
	CalendarICS    string `json:"calendarIcs"`
	CalendarMethod string `json:"calendarMethod"`
}

// internalSend lets a trusted sibling daemon (icaly) send mail on behalf of a local user. It is
// authenticated solely by the shared internal secret; the From user must be a real local
// account. icaly already enforces hp_icaly_invite AND hp_mail_send before calling this.
func (s *Server) internalSend(w http.ResponseWriter, r *http.Request) {
	if s.internalSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "Internal send not configured")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Mail-Internal-Secret")), []byte(s.internalSecret)) != 1 {
		writeErr(w, http.StatusUnauthorized, "Not authenticated")
		return
	}
	var req internalSendReq
	if !decodeBody(w, r, maxComposeBytes, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	fromUser := strings.ToLower(strings.TrimSpace(req.From))
	if fromUser == "" {
		writeErr(w, http.StatusBadRequest, "A from user is required")
		return
	}
	// The on-behalf-of user must exist as a real OS account (parity with how sessions resolve).
	if _, err := user.Lookup(fromUser); err != nil {
		writeErr(w, http.StatusForbidden, "Unknown sender")
		return
	}
	if len(cleanAddrs(req.To))+len(cleanAddrs(req.Cc)) == 0 {
		writeErr(w, http.StatusBadRequest, "At least one recipient is required")
		return
	}
	res, err := s.lda.Send(lda.SendInput{
		FromUser:       fromUser,
		To:             cleanAddrs(req.To),
		Cc:             cleanAddrs(req.Cc),
		Subject:        req.Subject,
		Body:           req.Body,
		CalendarICS:    req.CalendarICS,
		CalendarMethod: req.CalendarMethod,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not send message")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ── view + helpers ─────────────────────────────────────────────────────────────────

type msgView struct {
	ID             string `json:"id"`
	Folder         string `json:"folder"`
	From           string `json:"from"`
	To             string `json:"to"`
	Subject        string `json:"subject"`
	Date           string `json:"date"`
	Seen           bool   `json:"seen"`
	Flagged        bool   `json:"flagged"`
	Answered       bool   `json:"answered"`
	HasAttachments bool   `json:"hasAttachments"`
	Size           int64  `json:"size"`
	MessageID      string `json:"messageId"`
}

func toView(m maildir.Message) msgView {
	return msgView{
		ID: encodeID(m.ID), Folder: m.Folder, From: m.From, To: m.To, Subject: m.Subject,
		Date: rfc3339(m.Date), Seen: m.Seen, Flagged: m.Flagged, Answered: m.Answered,
		HasAttachments: m.HasAttachments, Size: m.Size, MessageID: m.MessageID,
	}
}

// attachmentView is the per-attachment metadata the reading pane shows; bytes are fetched
// separately via the attachment endpoint (referenced by index).
type attachmentView struct {
	Index       int    `json:"index"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
	Inline      bool   `json:"inline"`
	ContentID   string `json:"contentId,omitempty"`
}

func attachmentViews(atts []message.Attachment) []attachmentView {
	out := make([]attachmentView, 0, len(atts))
	for i, a := range atts {
		out = append(out, attachmentView{
			Index: i, Filename: a.Filename, ContentType: a.ContentType,
			Size: a.Size, Inline: a.Inline, ContentID: a.ContentID,
		})
	}
	return out
}

func folderParam(r *http.Request) string {
	f := r.URL.Query().Get("mailbox")
	if f == "" {
		return "INBOX"
	}
	return f
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// encodeID/decodeID make a maildir base name URL-safe for query params.
func encodeID(id string) string { return base64.RawURLEncoding.EncodeToString([]byte(id)) }

func decodeID(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || strings.ContainsRune(string(b), '/') {
		return "", false
	}
	return string(b), true
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func cleanAddrs(in []string) []string {
	var out []string
	for _, a := range in {
		t := strings.TrimSpace(a)
		// Drop anything carrying CR/LF — a recipient string is never multi-line, and allowing
		// it through would smuggle forged headers into the composed message.
		if t != "" && !strings.ContainsAny(t, "\r\n") {
			out = append(out, t)
		}
	}
	return out
}

// userExists reports whether name is a real local OS account (a provisionable holistic user).
func userExists(name string) bool {
	if name == "" {
		return false
	}
	_, err := user.Lookup(name)
	return err == nil
}

// decodeAttachments turns the base64 compose payloads into message attachments, capping the
// total decoded size so a compose cannot blow past the body limit after expansion.
func decodeAttachments(in []attachmentIn) ([]message.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]message.Attachment, 0, len(in))
	total := 0
	for _, a := range in {
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(a.Data))
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > maxComposeBytes {
			return nil, errors.New("attachments too large")
		}
		name := sanitizeFilename(a.Filename)
		if name == "" {
			name = "attachment"
		}
		ct := strings.TrimSpace(a.ContentType)
		if ct == "" {
			ct = "application/octet-stream"
		}
		out = append(out, message.Attachment{Filename: name, ContentType: ct, Size: len(data), Data: data})
	}
	return out, nil
}

// formatAttachmentDisposition builds a safe "Content-Disposition: attachment" value. The filename
// is sanitised and RFC 2231-encoded when non-ASCII; a missing name falls back to the index.
func formatAttachmentDisposition(filename string, idx int) string {
	name := sanitizeFilename(filename)
	if name == "" {
		name = "attachment-" + strconv.Itoa(idx)
	}
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment"
}

// inlineSafe reports whether a content type may be served inline (rendered in the browser) without
// risking script execution in our origin. SVG and HTML/XML are deliberately excluded — they can
// carry script — so they are always force-downloaded.
func inlineSafe(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case ct == "image/svg+xml":
		return false
	case strings.HasPrefix(ct, "image/"):
		return true
	case ct == "application/pdf":
		return true
	case strings.HasPrefix(ct, "audio/"), strings.HasPrefix(ct, "video/"):
		return true
	case ct == "text/plain":
		return true
	}
	return false
}

// sanitizeFilename strips control bytes and path separators so a crafted attachment name can
// neither break the header nor suggest a path.
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20:
			return -1
		case r == '"':
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}

func decodeBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil && err != io.EOF {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
