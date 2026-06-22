// Package jmap implements a pragmatic subset of JMAP (RFC 8620 core + RFC 8621 mail) on top
// of maild's Maildir store, so standards clients (aerc, Ltt.rs, Twake Mail) can read and
// manage mail over the HTTP-only tunnel. Scope: Session, Core/echo, Mailbox/get + /query,
// Email/query + /get + /set (keywords, mailbox moves, destroy). Composing/sending is done via
// the web API for now; EmailSubmission is a later addition. Auth is the holistic session OR a
// maild app password (HTTP Basic) — handled by the api layer, not here.
package jmap

import (
	"encoding/base64"
	"encoding/json"
	"net/mail"
	"strings"

	"mail/internal/instance"
	"mail/internal/maildir"
	"mail/internal/message"
)

const (
	capCore = "urn:ietf:params:jmap:core"
	capMail = "urn:ietf:params:jmap:mail"
	apiPath = "/api/services/mail/jmap/api"
)

// folderRoles maps maild's folders to JMAP mailbox roles.
var folderRoles = map[string]string{"INBOX": "inbox", "Sent": "sent", "Drafts": "drafts", "Trash": "trash"}

// Server serves JMAP for one maild instance.
type Server struct {
	store *maildir.Store
	inst  *instance.Resolver
}

// New builds a JMAP server.
func New(store *maildir.Store, inst *instance.Resolver) *Server {
	return &Server{store: store, inst: inst}
}

// Session returns the JMAP Session object for a user (the account id is the username).
func (s *Server) Session(user string) map[string]any {
	acct := user
	return map[string]any{
		"capabilities": map[string]any{
			capCore: map[string]any{
				"maxSizeUpload":         50 << 20,
				"maxConcurrentUpload":   4,
				"maxSizeRequestObject":  10 << 20,
				"maxConcurrentRequests": 4,
				"maxCallsInRequest":     32,
				"maxObjectsInGet":       512,
				"maxObjectsInSet":       512,
				"collationAlgorithms":   []string{},
			},
			capMail: map[string]any{},
		},
		"accounts": map[string]any{
			acct: map[string]any{
				"name":        s.inst.Address(user),
				"isPersonal":  true,
				"isReadOnly":  false,
				"accountCapabilities": map[string]any{
					capMail: map[string]any{
						"maxMailboxesPerEmail":       1,
						"maxSizeAttachmentsPerEmail": 25 << 20,
						"emailQuerySortOptions":      []string{"receivedAt"},
						"mayCreateTopLevelMailbox":   false,
					},
				},
			},
		},
		"primaryAccounts": map[string]any{capCore: acct, capMail: acct},
		"username":        user,
		"apiUrl":          apiPath,
		"downloadUrl":     apiPath + "/download/{accountId}/{blobId}/{name}",
		"uploadUrl":       apiPath + "/upload/{accountId}/",
		"eventSourceUrl":  apiPath + "/eventsource/",
		"state":           "0",
	}
}

type jmapRequest struct {
	Using       []string          `json:"using"`
	MethodCalls []json.RawMessage `json:"methodCalls"`
}

// API processes a JMAP request for user and returns the response body.
func (s *Server) API(user string, body []byte) ([]byte, int) {
	var req jmapRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return mustJSON(map[string]any{"type": "urn:ietf:params:jmap:error:notJSON"}), 400
	}
	responses := make([]any, 0, len(req.MethodCalls))
	for _, rawCall := range req.MethodCalls {
		var call []json.RawMessage
		if err := json.Unmarshal(rawCall, &call); err != nil || len(call) != 3 {
			continue
		}
		var name, callID string
		var args map[string]any
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[1], &args)
		_ = json.Unmarshal(call[2], &callID)
		if args == nil {
			args = map[string]any{}
		}
		resName, result := s.dispatch(user, name, args)
		responses = append(responses, []any{resName, result, callID})
	}
	return mustJSON(map[string]any{"methodResponses": responses, "sessionState": "0"}), 200
}

func (s *Server) dispatch(user, name string, args map[string]any) (string, any) {
	switch name {
	case "Core/echo":
		return "Core/echo", args
	case "Mailbox/get":
		return "Mailbox/get", s.mailboxGet(user, args)
	case "Mailbox/query":
		return "Mailbox/query", s.mailboxQuery(user)
	case "Email/query":
		return "Email/query", s.emailQuery(user, args)
	case "Email/get":
		return "Email/get", s.emailGet(user, args)
	case "Email/set":
		return "Email/set", s.emailSet(user, args)
	default:
		return "error", map[string]any{"type": "unknownMethod"}
	}
}

func (s *Server) mailboxGet(user string, args map[string]any) map[string]any {
	folders, _ := s.store.Folders(user)
	want := idFilter(args["ids"])
	list := make([]map[string]any, 0, len(folders))
	for i, f := range folders {
		if want != nil && !want[f.Name] {
			continue
		}
		list = append(list, map[string]any{
			"id":            f.Name,
			"name":          f.Name,
			"parentId":      nil,
			"role":          folderRoles[f.Name],
			"sortOrder":     i,
			"totalEmails":   f.Total,
			"unreadEmails":  f.Unread,
			"totalThreads":  f.Total,
			"unreadThreads": f.Unread,
			"myRights": map[string]any{
				"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true,
				"maySetSeen": true, "maySetKeywords": true, "mayDelete": false,
			},
			"isSubscribed": true,
		})
	}
	return map[string]any{"accountId": user, "state": "0", "list": list, "notFound": []string{}}
}

func (s *Server) mailboxQuery(user string) map[string]any {
	ids := make([]string, 0, len(maildir.StdFolders))
	ids = append(ids, maildir.StdFolders...)
	return map[string]any{
		"accountId": user, "queryState": "0", "canCalculateChanges": false,
		"position": 0, "total": len(ids), "ids": ids,
	}
}

func (s *Server) emailQuery(user string, args map[string]any) map[string]any {
	folder := "INBOX"
	if f, ok := args["filter"].(map[string]any); ok {
		if mb, ok := f["inMailbox"].(string); ok && maildir.ValidFolder(mb) {
			folder = mb
		}
	}
	msgs, _ := s.store.List(user, folder)
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, encodeID(folder, m.ID))
	}
	return map[string]any{
		"accountId": user, "queryState": "0", "canCalculateChanges": false,
		"position": 0, "total": len(ids), "ids": ids,
	}
}

func (s *Server) emailGet(user string, args map[string]any) map[string]any {
	ids := stringList(args["ids"])
	list := make([]map[string]any, 0, len(ids))
	notFound := []string{}
	for _, eid := range ids {
		folder, mid, ok := decodeID(eid)
		if !ok {
			notFound = append(notFound, eid)
			continue
		}
		meta, raw, err := s.store.Get(user, folder, mid)
		if err != nil {
			notFound = append(notFound, eid)
			continue
		}
		parsed, _ := message.Parse(raw)
		list = append(list, emailObject(eid, folder, meta, parsed))
	}
	return map[string]any{"accountId": user, "state": "0", "list": list, "notFound": notFound}
}

func (s *Server) emailSet(user string, args map[string]any) map[string]any {
	updated := map[string]any{}
	notUpdated := map[string]any{}
	destroyed := []string{}
	notDestroyed := map[string]any{}

	if upd, ok := args["update"].(map[string]any); ok {
		for eid, p := range upd {
			patch, _ := p.(map[string]any)
			if err := s.applyPatch(user, eid, patch); err != nil {
				notUpdated[eid] = map[string]any{"type": "invalidProperties", "description": err.Error()}
			} else {
				updated[eid] = nil
			}
		}
	}
	for _, eid := range stringList(args["destroy"]) {
		folder, mid, ok := decodeID(eid)
		if !ok {
			notDestroyed[eid] = map[string]any{"type": "notFound"}
			continue
		}
		if err := s.store.Delete(user, folder, mid); err != nil {
			notDestroyed[eid] = map[string]any{"type": "notFound"}
		} else {
			destroyed = append(destroyed, eid)
		}
	}
	return map[string]any{
		"accountId": user, "oldState": "0", "newState": "0",
		"updated": updated, "notUpdated": notUpdated,
		"destroyed": destroyed, "notDestroyed": notDestroyed,
	}
}

func (s *Server) applyPatch(user, eid string, patch map[string]any) error {
	folder, mid, ok := decodeID(eid)
	if !ok {
		return errNotFound
	}
	// Keyword changes (both "keywords/$seen": bool and a full "keywords" object).
	var seen, flagged, answered *bool
	setKW := func(name string, v bool) {
		switch name {
		case "$seen":
			seen = boolp(v)
		case "$flagged":
			flagged = boolp(v)
		case "$answered":
			answered = boolp(v)
		}
	}
	for k, v := range patch {
		switch {
		case strings.HasPrefix(k, "keywords/"):
			setKW(strings.TrimPrefix(k, "keywords/"), truthy(v))
		case k == "keywords":
			kw, _ := v.(map[string]any)
			seen, flagged, answered = boolp(has(kw, "$seen")), boolp(has(kw, "$flagged")), boolp(has(kw, "$answered"))
		}
	}
	if seen != nil || flagged != nil || answered != nil {
		if err := s.store.SetFlags(user, folder, mid, maildir.Flags{Seen: seen, Flagged: flagged, Answered: answered}); err != nil {
			return err
		}
	}
	// Mailbox move: a full "mailboxIds" object with a single destination.
	if mb, ok := patch["mailboxIds"].(map[string]any); ok {
		dest := ""
		for id, v := range mb {
			if truthy(v) {
				dest = id
			}
		}
		if dest != "" && dest != folder && maildir.ValidFolder(dest) {
			if err := s.store.Move(user, folder, dest, mid); err != nil {
				return err
			}
		}
	}
	return nil
}

// ── email object + helpers ─────────────────────────────────────────────────────

func emailObject(eid, folder string, m maildir.Message, p message.Parsed) map[string]any {
	keywords := map[string]any{}
	if m.Seen {
		keywords["$seen"] = true
	}
	if m.Flagged {
		keywords["$flagged"] = true
	}
	if m.Answered {
		keywords["$answered"] = true
	}
	received := ""
	if !p.Date.IsZero() {
		received = p.Date.UTC().Format("2006-01-02T15:04:05Z")
	}
	obj := map[string]any{
		"id":          eid,
		"blobId":      eid,
		"threadId":    eid,
		"mailboxIds":  map[string]any{folder: true},
		"keywords":    keywords,
		"size":        m.Size,
		"receivedAt":  received,
		"from":        addrList(p.From),
		"to":          addrList(p.To),
		"cc":          addrList(p.Cc),
		"subject":     p.Subject,
		"messageId":   nonEmpty(p.MessageID),
		"inReplyTo":   nonEmpty(p.InReplyTo),
		"references":  p.References,
		"preview":     preview(p.Text),
		"hasAttachment": false,
		"textBody": []map[string]any{
			{"partId": "text", "blobId": eid, "type": "text/plain", "size": len(p.Text)},
		},
		"bodyValues": map[string]any{
			"text": map[string]any{"value": p.Text, "isEncodingProblem": false, "isTruncated": false},
		},
	}
	return obj
}

func addrList(s string) []map[string]any {
	out := []map[string]any{}
	if strings.TrimSpace(s) == "" {
		return out
	}
	addrs, err := mail.ParseAddressList(s)
	if err != nil {
		return out
	}
	for _, a := range addrs {
		out = append(out, map[string]any{"name": a.Name, "email": a.Address})
	}
	return out
}

func preview(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if len(t) > 200 {
		t = t[:200]
	}
	return t
}

func encodeID(folder, mid string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(folder + "\x00" + mid))
}

func decodeID(eid string) (folder, mid string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(eid)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(b), "\x00", 2)
	if len(parts) != 2 || !maildir.ValidFolder(parts[0]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func idFilter(v any) map[string]bool {
	ids := stringList(v)
	if ids == nil {
		return nil
	}
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func has(m map[string]any, k string) bool { _, ok := m[k]; return ok }
func truthy(v any) bool                    { b, _ := v.(bool); return b }
func boolp(b bool) *bool                   { return &b }
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

type jmapError string

func (e jmapError) Error() string { return string(e) }

const errNotFound = jmapError("notFound")
