package jmap

import (
	"encoding/json"
	"testing"

	"mail/internal/instance"
	"mail/internal/maildir"
	"mail/internal/message"
)

func newServer(t *testing.T) (*Server, *maildir.Store) {
	t.Helper()
	t.Setenv("HOLISTIC_MAIL_DOMAIN", "example.test")
	store := maildir.New(t.TempDir())
	raw, _ := message.Build(message.BuildOptions{
		From: "alice@example.test", To: []string{"tester@example.test"},
		Subject: "JMAP hello", Body: "Body line one.\nBody line two.", Domain: "example.test",
	})
	if _, err := store.Deliver("tester", "INBOX", raw, false); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return New(store, instance.New()), store
}

// call runs one JMAP method and returns its result object.
func call(t *testing.T, srv *Server, user, method string, args map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"using":       []string{capCore, capMail},
		"methodCalls": []any{[]any{method, args, "c0"}},
	})
	resp, status := srv.API(user, body)
	if status != 200 {
		t.Fatalf("%s: status %d", method, status)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || len(out.MethodResponses) != 1 {
		t.Fatalf("%s: bad response %s", method, resp)
	}
	var name string
	_ = json.Unmarshal(out.MethodResponses[0][0], &name)
	if name == "error" {
		t.Fatalf("%s: jmap error %s", method, out.MethodResponses[0][1])
	}
	var res map[string]any
	_ = json.Unmarshal(out.MethodResponses[0][1], &res)
	return res
}

func TestSession(t *testing.T) {
	srv, _ := newServer(t)
	sess := srv.Session("tester")
	if sess["apiUrl"] != apiPath {
		t.Errorf("apiUrl = %v", sess["apiUrl"])
	}
	accts, _ := sess["accounts"].(map[string]any)
	if _, ok := accts["tester"]; !ok {
		t.Errorf("session missing account 'tester': %v", accts)
	}
	prim, _ := sess["primaryAccounts"].(map[string]any)
	if prim[capMail] != "tester" {
		t.Errorf("primary mail account = %v", prim[capMail])
	}
}

func TestMailboxAndEmailFlow(t *testing.T) {
	srv, _ := newServer(t)

	// Mailbox/get → INBOX present with one unread.
	mb := call(t, srv, "tester", "Mailbox/get", map[string]any{"accountId": "tester", "ids": nil})
	var inbox map[string]any
	for _, e := range mb["list"].([]any) {
		m := e.(map[string]any)
		if m["id"] == "INBOX" {
			inbox = m
		}
	}
	if inbox == nil {
		t.Fatalf("no INBOX mailbox: %v", mb["list"])
	}
	if inbox["unreadEmails"].(float64) != 1 {
		t.Errorf("INBOX unread = %v, want 1", inbox["unreadEmails"])
	}

	// Email/query → one id.
	q := call(t, srv, "tester", "Email/query", map[string]any{"accountId": "tester", "filter": map[string]any{"inMailbox": "INBOX"}})
	ids := q["ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("Email/query ids = %v, want 1", ids)
	}
	id := ids[0].(string)

	// Email/get → headers + body; not yet seen.
	g := call(t, srv, "tester", "Email/get", map[string]any{"accountId": "tester", "ids": []any{id}})
	em := g["list"].([]any)[0].(map[string]any)
	if em["subject"] != "JMAP hello" {
		t.Errorf("subject = %v", em["subject"])
	}
	if _, seen := em["keywords"].(map[string]any)["$seen"]; seen {
		t.Errorf("message should not be $seen yet")
	}
	from := em["from"].([]any)
	if len(from) != 1 || from[0].(map[string]any)["email"] != "alice@example.test" {
		t.Errorf("from = %v", from)
	}

	// Email/set → mark $seen.
	set := call(t, srv, "tester", "Email/set", map[string]any{
		"accountId": "tester",
		"update":    map[string]any{id: map[string]any{"keywords/$seen": true}},
	})
	if _, ok := set["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("Email/set did not update %s: %v", id, set)
	}

	// Re-get → now $seen.
	g2 := call(t, srv, "tester", "Email/get", map[string]any{"accountId": "tester", "ids": []any{id}})
	em2 := g2["list"].([]any)[0].(map[string]any)
	if _, seen := em2["keywords"].(map[string]any)["$seen"]; !seen {
		t.Errorf("message should be $seen after Email/set: %v", em2["keywords"])
	}

	// Email/set → move to Trash.
	mv := call(t, srv, "tester", "Email/set", map[string]any{
		"accountId": "tester",
		"update":    map[string]any{id: map[string]any{"mailboxIds": map[string]any{"Trash": true}}},
	})
	if _, ok := mv["updated"].(map[string]any)[id]; !ok {
		t.Fatalf("move did not update: %v", mv)
	}
	q2 := call(t, srv, "tester", "Email/query", map[string]any{"accountId": "tester", "filter": map[string]any{"inMailbox": "INBOX"}})
	if len(q2["ids"].([]any)) != 0 {
		t.Errorf("INBOX should be empty after move, got %v", q2["ids"])
	}
}

func TestUnknownMethod(t *testing.T) {
	srv, _ := newServer(t)
	body, _ := json.Marshal(map[string]any{
		"using":       []string{capCore},
		"methodCalls": []any{[]any{"Frobnicate/now", map[string]any{}, "c0"}},
	})
	resp, status := srv.API("tester", body)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	var out struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	_ = json.Unmarshal(resp, &out)
	var name string
	_ = json.Unmarshal(out.MethodResponses[0][0], &name)
	if name != "error" {
		t.Errorf("unknown method should return error, got %s", name)
	}
}
