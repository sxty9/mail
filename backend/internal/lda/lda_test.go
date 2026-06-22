package lda

import (
	"os/user"
	"testing"

	"mail/internal/instance"
	"mail/internal/maildir"
	"mail/internal/message"
	"mail/internal/outbound"
)

// newTestDeliverer wires a Deliverer against a temp Maildir root and a fixed mail domain.
func newTestDeliverer(t *testing.T) (*Deliverer, *maildir.Store, string) {
	t.Helper()
	t.Setenv("HOLISTIC_MAIL_DOMAIN", "example.test")
	root := t.TempDir()
	store := maildir.New(root)
	out := outbound.New(t.TempDir(), "", "") // no edge configured
	del := New(store, out, instance.New())

	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	return del, store, me.Username
}

func count(t *testing.T, store *maildir.Store, user, folder string) int {
	t.Helper()
	msgs, err := store.List(user, folder)
	if err != nil {
		t.Fatalf("list %s: %v", folder, err)
	}
	return len(msgs)
}

func TestSendLocalAndExternal(t *testing.T) {
	del, store, me := newTestDeliverer(t)

	res, err := del.Send(SendInput{
		FromUser: me,
		To:       []string{me + "@example.test", "stranger@elsewhere.test"},
		Subject:  "Hallo Holistic",
		Body:     "Erste interne Mail.\nZeile zwei.",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.DeliveredLocal != 1 {
		t.Errorf("DeliveredLocal = %d, want 1", res.DeliveredLocal)
	}
	if res.QueuedExternal != 1 {
		t.Errorf("QueuedExternal = %d, want 1", res.QueuedExternal)
	}
	if got := count(t, store, me, "INBOX"); got != 1 {
		t.Errorf("INBOX count = %d, want 1", got)
	}
	if got := count(t, store, me, "Sent"); got != 1 {
		t.Errorf("Sent count = %d, want 1", got)
	}

	// The delivered message must be unread and carry the subject.
	inbox, _ := store.List(me, "INBOX")
	if inbox[0].Seen {
		t.Errorf("delivered message should be unread")
	}
	if inbox[0].Subject != "Hallo Holistic" {
		t.Errorf("subject = %q", inbox[0].Subject)
	}
	// The Sent copy must be marked seen.
	sent, _ := store.List(me, "Sent")
	if !sent[0].Seen {
		t.Errorf("sent copy should be seen")
	}
}

func TestDeliverInbound(t *testing.T) {
	del, store, me := newTestDeliverer(t)

	raw, _ := message.Build(message.BuildOptions{
		From:    "outside@elsewhere.test",
		To:      []string{me + "@example.test"},
		Subject: "Von außen",
		Body:    "Inbound über die Edge.",
		Domain:  "elsewhere.test",
	})
	n, err := del.DeliverInbound([]string{me + "@example.test"}, raw)
	if err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if n != 1 {
		t.Errorf("delivered = %d, want 1", n)
	}
	if got := count(t, store, me, "INBOX"); got != 1 {
		t.Errorf("INBOX count = %d, want 1", got)
	}

	// Unknown recipient → rejected (use a name that cannot exist as an OS account).
	if _, err := del.DeliverInbound([]string{"zzznouser4242@example.test"}, raw); err == nil {
		t.Errorf("expected error for unknown recipient")
	}
}

func TestFlagsAndMoveAndDelete(t *testing.T) {
	del, store, me := newTestDeliverer(t)
	if _, err := del.Send(SendInput{FromUser: me, To: []string{me + "@example.test"}, Subject: "x", Body: "y"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	inbox, _ := store.List(me, "INBOX")
	id := inbox[0].ID

	yes := true
	if err := store.SetFlags(me, "INBOX", id, maildir.Flags{Seen: &yes}); err != nil {
		t.Fatalf("setflags: %v", err)
	}
	inbox, _ = store.List(me, "INBOX")
	if !inbox[0].Seen {
		t.Errorf("message should be seen after SetFlags")
	}

	if err := store.Move(me, "INBOX", "Trash", id); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := count(t, store, me, "INBOX"); got != 0 {
		t.Errorf("INBOX after move = %d, want 0", got)
	}
	if got := count(t, store, me, "Trash"); got != 1 {
		t.Errorf("Trash after move = %d, want 1", got)
	}

	// Deleting from Trash removes permanently.
	if err := store.Delete(me, "Trash", id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := count(t, store, me, "Trash"); got != 0 {
		t.Errorf("Trash after delete = %d, want 0", got)
	}
}
