package maildir

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidFolderLexical pins the traversal guard with hierarchy support: well-formed flat AND
// nested names pass; anything with a Maildir++ dot, a leading dot, an empty/".."/control-byte
// segment, over-length, or over-depth is rejected.
func TestValidFolderLexical(t *testing.T) {
	for _, g := range []string{"INBOX", "Sent", "Drafts", "Trash", "Projects", "My Folder", "a", "A1_b-c", "Work/Projects", "a/b/c/d/e"} {
		if !ValidFolder(g) {
			t.Errorf("ValidFolder(%q) = false, want true", g)
		}
	}
	for _, b := range []string{
		"", ".", "..", "../x", ".hidden", "Sent/../x", "tab\tname", strings.Repeat("x", 65),
		"dot.name", "a//b", "a/", "/a", "a/.hidden", "a/b/c/d/e/f", // depth 6 > cap
	} {
		if ValidFolder(b) {
			t.Errorf("ValidFolder(%q) = true, want false", b)
		}
	}
}

// TestCustomFolderCRUD covers the flat lifecycle: create (with reserved/invalid rejection),
// discovery as a custom folder, rename keeping messages, and delete relocating messages to Trash.
func TestCustomFolderCRUD(t *testing.T) {
	store := New(t.TempDir())
	u := "tester"
	if err := store.EnsureUser(u); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"INBOX", "Sent", "dot.name", "a//b", ""} {
		if err := store.CreateFolder(u, bad); !errors.Is(err, ErrInvalidFolder) {
			t.Errorf("CreateFolder(%q) err = %v, want ErrInvalidFolder", bad, err)
		}
	}

	if err := store.CreateFolder(u, "Projects"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CreateFolder(u, "Projects"); !errors.Is(err, ErrFolderExists) {
		t.Errorf("duplicate create err = %v, want ErrFolderExists", err)
	}
	if !store.HasFolder(u, "Projects") {
		t.Error("HasFolder(Projects) = false after create")
	}

	folders, _ := store.Folders(u)
	var info *FolderInfo
	for i := range folders {
		if folders[i].Name == "Projects" {
			info = &folders[i]
		}
	}
	if info == nil || !info.Custom {
		t.Fatalf("Projects not listed as a custom folder: %+v", folders)
	}

	if _, err := store.Deliver(u, "Projects", []byte("Subject: hi\r\n\r\nbody"), false); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if c, _ := store.List(u, "Projects"); len(c) != 1 {
		t.Fatalf("Projects has %d msgs, want 1", len(c))
	}

	// Rename carries the message across.
	if err := store.RenameFolder(u, "Projects", "Work"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if store.HasFolder(u, "Projects") {
		t.Error("old folder still present after rename")
	}
	if c, _ := store.List(u, "Work"); len(c) != 1 {
		t.Fatalf("Work has %d msgs after rename, want 1", len(c))
	}

	// Delete relocates the message to Trash rather than dropping it.
	if err := store.DeleteFolder(u, "Work"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if store.HasFolder(u, "Work") {
		t.Error("folder still present after delete")
	}
	if c, _ := store.List(u, "Trash"); len(c) != 1 {
		t.Fatalf("Trash has %d msgs after folder delete, want 1", len(c))
	}

	// Standard folders are immutable.
	if err := store.DeleteFolder(u, "Sent"); !errors.Is(err, ErrInvalidFolder) {
		t.Errorf("DeleteFolder(Sent) err = %v, want ErrInvalidFolder", err)
	}
	if err := store.RenameFolder(u, "Sent", "Outbox"); !errors.Is(err, ErrInvalidFolder) {
		t.Errorf("RenameFolder(Sent) err = %v, want ErrInvalidFolder", err)
	}
}

// TestNestedFolders covers the hierarchy: creating a child mkdir-p's its parent, both are
// discovered as wire names, renaming a parent cascades to the whole subtree, and deleting a parent
// relocates every descendant's messages to Trash and removes the subtree.
func TestNestedFolders(t *testing.T) {
	store := New(t.TempDir())
	u := "tester"
	if err := store.EnsureUser(u); err != nil {
		t.Fatal(err)
	}

	// Creating a child materialises its missing ancestor.
	if err := store.CreateFolder(u, "Work/Projects"); err != nil {
		t.Fatalf("create nested: %v", err)
	}
	if !store.HasFolder(u, "Work") {
		t.Error("ancestor Work not auto-created")
	}
	if !store.HasFolder(u, "Work/Projects") {
		t.Error("child Work/Projects missing")
	}
	if _, err := store.Deliver(u, "Work/Projects", []byte("Subject: x\r\n\r\nb"), false); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// A folder cannot be moved inside itself.
	if err := store.RenameFolder(u, "Work", "Work/Sub"); !errors.Is(err, ErrInvalidFolder) {
		t.Errorf("RenameFolder(Work → Work/Sub) err = %v, want ErrInvalidFolder", err)
	}

	// Renaming the parent cascades to the child (subtree rename).
	if err := store.RenameFolder(u, "Work", "Job"); err != nil {
		t.Fatalf("rename parent: %v", err)
	}
	if store.HasFolder(u, "Work") || store.HasFolder(u, "Work/Projects") {
		t.Error("old subtree still present after parent rename")
	}
	if !store.HasFolder(u, "Job") || !store.HasFolder(u, "Job/Projects") {
		t.Error("renamed subtree missing")
	}
	if c, _ := store.List(u, "Job/Projects"); len(c) != 1 {
		t.Fatalf("Job/Projects has %d msgs after rename, want 1", len(c))
	}

	// Deleting the parent relocates the child's message to Trash and removes the whole subtree.
	if err := store.DeleteFolder(u, "Job"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if store.HasFolder(u, "Job") || store.HasFolder(u, "Job/Projects") {
		t.Error("subtree still present after parent delete")
	}
	if c, _ := store.List(u, "Trash"); len(c) != 1 {
		t.Fatalf("Trash has %d msgs after subtree delete, want 1", len(c))
	}
}

// TestFolderOrdering pins the persisted display order: SetFolderOrder is honoured by Folders, an
// unknown name is dropped, and a custom folder missing from the request is appended.
func TestFolderOrdering(t *testing.T) {
	store := New(t.TempDir())
	u := "tester"
	for _, n := range []string{"Alpha", "Beta", "Gamma"} {
		if err := store.CreateFolder(u, n); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	if err := store.SetFolderOrder(u, []string{"Gamma", "Alpha", "ghost"}); err != nil {
		t.Fatalf("set order: %v", err)
	}
	got := customOrder(t, store, u)
	// Gamma, Alpha explicit; Beta (omitted) appended alphabetically; ghost (unknown) dropped.
	if want := []string{"Gamma", "Alpha", "Beta"}; !equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestPurgeExpiredTrash covers Trash auto-expiry: a freshly-deleted message survives, one whose
// deletion is older than the retention window is purged, purging is scoped to Trash (INBOX is never
// touched), and a retention <= 0 is a no-op.
func TestPurgeExpiredTrash(t *testing.T) {
	store := New(t.TempDir())
	u := "tester"

	// Two messages in INBOX; delete both into Trash.
	for _, subj := range []string{"old", "fresh"} {
		if _, err := store.Deliver(u, "INBOX", []byte("Subject: "+subj+"\r\n\r\nbody"), false); err != nil {
			t.Fatalf("deliver %s: %v", subj, err)
		}
	}
	msgs, _ := store.List(u, "INBOX")
	if len(msgs) != 2 {
		t.Fatalf("INBOX has %d msgs, want 2", len(msgs))
	}
	for _, m := range msgs {
		if err := store.Delete(u, "INBOX", m.ID); err != nil {
			t.Fatalf("delete %s: %v", m.ID, err)
		}
	}
	if c, _ := store.List(u, "Trash"); len(c) != 2 {
		t.Fatalf("Trash has %d msgs after delete, want 2", len(c))
	}

	// Backdate exactly one Trash message to 40 days ago.
	trashNew := filepath.Join(folderDir(store.userRoot(u), "Trash"), "new")
	entries, _ := os.ReadDir(trashNew)
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(trashNew, entries[0].Name()), old, old); err != nil {
		t.Fatal(err)
	}

	// retention <= 0 is a no-op.
	if n, err := store.PurgeExpiredTrash(0); err != nil || n != 0 {
		t.Fatalf("PurgeExpiredTrash(0) = %d, %v; want 0, nil", n, err)
	}
	if c, _ := store.List(u, "Trash"); len(c) != 2 {
		t.Fatalf("Trash has %d msgs after no-op purge, want 2", len(c))
	}

	// 30-day retention removes the backdated one, keeps the fresh one.
	n, err := store.PurgeExpiredTrash(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	if c, _ := store.List(u, "Trash"); len(c) != 1 {
		t.Errorf("Trash has %d msgs after purge, want 1", len(c))
	}
}

// TestPurgeKeepsRecentlyDeletedOldMail guards the stamping: an old message (received long ago) that
// is deleted *now* must NOT be purged immediately — the 30-day clock runs from deletion, not receipt.
func TestPurgeKeepsRecentlyDeletedOldMail(t *testing.T) {
	store := New(t.TempDir())
	u := "tester"
	if _, err := store.Deliver(u, "INBOX", []byte("Subject: ancient\r\n\r\nbody"), false); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	msgs, _ := store.List(u, "INBOX")
	// Backdate the message in INBOX to a year ago (an old, long-received mail).
	inboxNew := filepath.Join(store.userRoot(u), "new")
	entries, _ := os.ReadDir(inboxNew)
	ancient := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(inboxNew, entries[0].Name()), ancient, ancient); err != nil {
		t.Fatal(err)
	}
	// Delete it now — stampNow should reset its clock to the present.
	if err := store.Delete(u, "INBOX", msgs[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, err := store.PurgeExpiredTrash(30 * 24 * time.Hour); err != nil || n != 0 {
		t.Fatalf("purge removed %d (err=%v); a just-deleted mail must survive despite its old receive date", n, err)
	}
	if c, _ := store.List(u, "Trash"); len(c) != 1 {
		t.Errorf("Trash has %d msgs, want 1", len(c))
	}
}

func customOrder(t *testing.T, s *Store, u string) []string {
	t.Helper()
	folders, err := s.Folders(u)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range folders {
		if f.Custom {
			out = append(out, f.Name)
		}
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMarkAllReadAndEmptyTrash covers the two bulk folder actions behind the right-click menu:
// MarkAllRead flips every unread message to seen (skipping already-read ones and idempotent on a
// second sweep), and EmptyTrash permanently purges Trash (and is a clean no-op when already empty).
func TestMarkAllReadAndEmptyTrash(t *testing.T) {
	store := New(t.TempDir())
	u := "tester"
	if err := store.EnsureUser(u); err != nil {
		t.Fatal(err)
	}

	// Three unread + one already-seen: MarkAllRead must touch only the three unread.
	for i := 0; i < 3; i++ {
		if _, err := store.Deliver(u, "INBOX", []byte("Subject: m\r\n\r\nbody"), false); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}
	if _, err := store.Deliver(u, "INBOX", []byte("Subject: seen\r\n\r\nbody"), true); err != nil {
		t.Fatalf("deliver seen: %v", err)
	}

	n, err := store.MarkAllRead(u, "INBOX")
	if err != nil {
		t.Fatalf("MarkAllRead: %v", err)
	}
	if n != 3 {
		t.Errorf("MarkAllRead marked %d, want 3", n)
	}
	msgs, _ := store.List(u, "INBOX")
	if len(msgs) != 4 {
		t.Fatalf("INBOX has %d msgs, want 4", len(msgs))
	}
	for _, m := range msgs {
		if !m.Seen {
			t.Errorf("message %s still unread after MarkAllRead", m.ID)
		}
	}
	if again, _ := store.MarkAllRead(u, "INBOX"); again != 0 {
		t.Errorf("second MarkAllRead marked %d, want 0 (idempotent)", again)
	}

	// EmptyTrash: relocate two into Trash, then purge them for good.
	for _, m := range msgs[:2] {
		if err := store.Move(u, "INBOX", "Trash", m.ID); err != nil {
			t.Fatalf("move to Trash: %v", err)
		}
	}
	if c, _ := store.List(u, "Trash"); len(c) != 2 {
		t.Fatalf("Trash has %d before empty, want 2", len(c))
	}
	removed, err := store.EmptyTrash(u)
	if err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	if removed != 2 {
		t.Errorf("EmptyTrash removed %d, want 2", removed)
	}
	if c, _ := store.List(u, "Trash"); len(c) != 0 {
		t.Fatalf("Trash has %d after empty, want 0", len(c))
	}
	if removed, err := store.EmptyTrash(u); err != nil || removed != 0 {
		t.Errorf("EmptyTrash on empty = (%d, %v), want (0, nil)", removed, err)
	}
}
