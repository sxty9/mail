// Package api serves maild's HTTP surface under /api/services/mail/, behind the shared
// holistic session. Reads/manage are gated by hp_mail_read, sending by hp_mail_send, both
// scoped to the caller's OWN mailbox. The inbound webhook is the one machine-to-machine
// endpoint: it is authenticated by a shared secret (the sxgate mail edge calls it), not the
// user JWT. Error bodies follow holistic's contract: {"detail": "..."}.
package api

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"mail/internal/apppass"
	"mail/internal/auth"
	"mail/internal/instance"
	"mail/internal/jmap"
	"mail/internal/lda"
	"mail/internal/maildir"
	"mail/internal/message"
	"mail/internal/rights"
)

const (
	base    = "/api/services/mail/"
	service = "mail"
	version = "0.1.0"

	maxComposeBytes = 30 << 20 // 30 MiB compose payload
	maxInboundBytes = 50 << 20 // 50 MiB inbound message
)

// Server wires the verifier, store and delivery agent into HTTP handlers.
type Server struct {
	v             *auth.Verifier
	store         *maildir.Store
	inst          *instance.Resolver
	lda           *lda.Deliverer
	apppass       *apppass.Store
	jmap          *jmap.Server
	inboundSecret string
}

// New builds a server.
func New(v *auth.Verifier, store *maildir.Store, inst *instance.Resolver, del *lda.Deliverer, ap *apppass.Store, inboundSecret string) *Server {
	return &Server{
		v: v, store: store, inst: inst, lda: del, apppass: ap,
		jmap: jmap.New(store, inst), inboundSecret: inboundSecret,
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
	mux.HandleFunc("POST "+base+"flags", s.guard(rights.GroupRead, true, s.flags))
	mux.HandleFunc("POST "+base+"move", s.guard(rights.GroupRead, true, s.move))
	mux.HandleFunc("POST "+base+"delete", s.guard(rights.GroupRead, true, s.delete))

	// Sending, gated by hp_mail_send.
	mux.HandleFunc("POST "+base+"send", s.guard(rights.GroupSend, true, s.send))

	// App passwords for native clients — managed from the browser session (CSRF-protected).
	mux.HandleFunc("GET "+base+"apppasswords", s.guard(rights.GroupRead, false, s.appList))
	mux.HandleFunc("POST "+base+"apppasswords", s.guard(rights.GroupRead, true, s.appCreate))
	mux.HandleFunc("POST "+base+"apppasswords/delete", s.guard(rights.GroupRead, true, s.appDelete))

	// JMAP — usable by the holistic session OR a native client with an app password (Basic auth).
	mux.HandleFunc("GET "+base+"jmap/session", s.guardMail(rights.GroupRead, s.jmapSession))
	mux.HandleFunc("POST "+base+"jmap/api", s.guardMail(rights.GroupRead, s.jmapAPI))

	// Inbound edge webhook — secret-authenticated, NOT the user session.
	mux.HandleFunc("POST "+base+"inbound", s.inbound)
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
		"address":    s.inst.Address(u.Username),
		"mailDomain": s.inst.MailDomain(),
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
	if !maildir.ValidFolder(folder) {
		writeErr(w, http.StatusBadRequest, "Unknown mailbox")
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
		"id":         r.URL.Query().Get("id"),
		"folder":     folder,
		"from":       p.From,
		"to":         p.To,
		"cc":         p.Cc,
		"subject":    p.Subject,
		"date":       rfc3339(p.Date),
		"messageId":  p.MessageID,
		"inReplyTo":  p.InReplyTo,
		"references": p.References,
		"text":       p.Text,
		"html":       p.HTML,
	})
}

type sendReq struct {
	To         []string `json:"to"`
	Cc         []string `json:"cc"`
	Subject    string   `json:"subject"`
	Body       string   `json:"body"`
	InReplyTo  string   `json:"inReplyTo"`
	References []string `json:"references"`
}

func (s *Server) send(w http.ResponseWriter, r *http.Request, u *auth.User) {
	var req sendReq
	if !decodeBody(w, r, maxComposeBytes, &req) {
		writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if len(cleanAddrs(req.To))+len(cleanAddrs(req.Cc)) == 0 {
		writeErr(w, http.StatusBadRequest, "At least one recipient is required")
		return
	}
	res, err := s.lda.Send(lda.SendInput{
		FromUser:   u.Username,
		To:         cleanAddrs(req.To),
		Cc:         cleanAddrs(req.Cc),
		Subject:    req.Subject,
		Body:       req.Body,
		InReplyTo:  strings.TrimSpace(req.InReplyTo),
		References: req.References,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not send message")
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "delivered": n})
}

// ── view + helpers ─────────────────────────────────────────────────────────────────

type msgView struct {
	ID        string `json:"id"`
	Folder    string `json:"folder"`
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	Date      string `json:"date"`
	Seen      bool   `json:"seen"`
	Flagged   bool   `json:"flagged"`
	Answered  bool   `json:"answered"`
	Size      int64  `json:"size"`
	MessageID string `json:"messageId"`
}

func toView(m maildir.Message) msgView {
	return msgView{
		ID: encodeID(m.ID), Folder: m.Folder, From: m.From, To: m.To, Subject: m.Subject,
		Date: rfc3339(m.Date), Seen: m.Seen, Flagged: m.Flagged, Answered: m.Answered,
		Size: m.Size, MessageID: m.MessageID,
	}
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
		if t := strings.TrimSpace(a); t != "" {
			out = append(out, t)
		}
	}
	return out
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
