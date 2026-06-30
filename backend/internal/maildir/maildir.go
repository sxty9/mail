// Package maildir is maild's message store: a per-user Maildir++ tree that IS the single
// source of truth for message bytes and flags (no database, matching the holistic
// "files + atomic writes" convention). A mailbox lives at <root>/<user>/Maildir with
// INBOX = the top dir and Sent/Drafts/Trash as ".Name" submaildirs. Delivery is the
// standard tmp→new (or tmp→cur) rename, which is atomic on a single filesystem.
package maildir

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mail/internal/message"
)

// StdFolders are the mailboxes every account has. INBOX is the maildir root; the rest are
// Maildir++ subfolders.
var StdFolders = []string{"INBOX", "Sent", "Drafts", "Trash"}

// folderSegmentRE constrains ONE path segment of a mailbox name to a traversal-safe label: it
// forbids the Maildir++ separator ".", the hierarchy/path separator "/", leading spaces/dots and
// control bytes, so a name can never escape the user's Maildir or break the wire→disk mapping.
// A full mailbox name is one or more such segments joined by "/" (the hierarchy delimiter), e.g.
// "Work/Projects/2026". Standard folders and INBOX are single segments and also satisfy it.
var folderSegmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$`)

// maxFolderDepth caps how deeply folders may nest (e.g. "a/b/c/d/e"), bounding both the on-disk
// name length and the UI tree.
const maxFolderDepth = 5

// folderOrderFile is the per-user file (a sibling of the Maildir) that persists the user's chosen
// display order for custom folders. Standard folders keep their fixed canonical order.
const folderOrderFile = "folders.json"

// Folder-management errors (custom folders only; the standard set is immutable).
var (
	ErrInvalidFolder = errors.New("invalid folder name")
	ErrFolderExists  = errors.New("folder already exists")
)

// Store is a set of Maildirs rooted at a single directory. mu serializes folder-tree mutations
// (deliver/flag/move/delete/folder create/rename/delete) against each other and against the
// readers (List/Get/ReadRaw/Folders/HasFolder), so a listing never observes a half-finished
// flag or move rename.
type Store struct {
	root string
	mu   sync.RWMutex
}

// New returns a store whose mailboxes live under root (e.g. /var/lib/mail/mailboxes).
func New(root string) *Store { return &Store{root: root} }

// Message is one message's metadata for a list view (no body).
type Message struct {
	ID             string    `json:"id"` // maildir base name (unique part, no :2, flags)
	Folder         string    `json:"folder"`
	From           string    `json:"from"`
	To             string    `json:"to"`
	Subject        string    `json:"subject"`
	Date           time.Time `json:"date"`
	MessageID      string    `json:"messageId"`
	InReplyTo      string    `json:"inReplyTo"`
	References     []string  `json:"references"`
	Seen           bool      `json:"seen"`
	Flagged        bool      `json:"flagged"`
	Answered       bool      `json:"answered"`
	HasAttachments bool      `json:"hasAttachments"`
	Size           int64     `json:"size"`
}

// FolderInfo is a mailbox plus its message counts. Custom marks a user-created folder (one the
// UI may rename or delete); the standard folders and INBOX are not custom.
type FolderInfo struct {
	Name   string `json:"name"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
	Custom bool   `json:"custom"`
}

func (s *Store) userDir(user string) string  { return filepath.Join(s.root, user) }
func (s *Store) userRoot(user string) string { return filepath.Join(s.root, user, "Maildir") }

// folderDir maps a wire mailbox name ("Work/Projects") to its on-disk Maildir++ directory
// (".Work.Projects" under the user root). Because no segment may contain "." or "/", replacing the
// hierarchy delimiter with the Maildir++ "." separator is unambiguous and reversible.
func folderDir(userRoot, folder string) string {
	if folder == "" || folder == "INBOX" {
		return userRoot
	}
	return filepath.Join(userRoot, "."+strings.ReplaceAll(folder, "/", "."))
}

// diskNameToFolder reverses the wire→disk mapping for discovery: ".Work.Projects" → "Work/Projects".
// Returns "" if the directory name is not a well-formed mailbox (stray dirs are ignored).
func diskNameToFolder(diskName string) string {
	if !strings.HasPrefix(diskName, ".") {
		return ""
	}
	name := strings.ReplaceAll(diskName[1:], ".", "/")
	if !validFolderName(name) {
		return ""
	}
	return name
}

// validFolderName reports whether name is a syntactically safe (possibly hierarchical) mailbox
// name: one or more "/"-joined segments, each matching folderSegmentRE, within the depth cap. It
// is the traversal guard — no segment can be "." or ".." (the segment regex requires a leading
// alphanumeric), so a name can never escape the user's Maildir.
func validFolderName(name string) bool {
	if name == "" {
		return false
	}
	segs := strings.Split(name, "/")
	if len(segs) > maxFolderDepth {
		return false
	}
	for _, seg := range segs {
		if !folderSegmentRE.MatchString(seg) {
			return false
		}
	}
	return true
}

// ancestors returns the ancestor folder names of a hierarchical name, shallowest first:
// "a/b/c" → ["a", "a/b"]. A top-level name has no ancestors.
func ancestors(name string) []string {
	segs := strings.Split(name, "/")
	out := make([]string, 0, len(segs)-1)
	for i := 1; i < len(segs); i++ {
		out = append(out, strings.Join(segs[:i], "/"))
	}
	return out
}

// ValidFolder reports whether folder is a syntactically safe mailbox name (guards path
// traversal). It is purely lexical — INBOX, the standard folders and any well-formed custom
// (possibly nested) name pass; existence is the store operation's concern.
func ValidFolder(folder string) bool {
	return validFolderName(folder)
}

// isStdFolder reports whether name is one of the immutable standard mailboxes (incl. INBOX).
func isStdFolder(name string) bool {
	for _, f := range StdFolders {
		if f == name {
			return true
		}
	}
	return false
}

// isCustomFolderName reports whether name is a valid name for a user-created folder: well-formed
// (possibly nested) and not colliding with a standard mailbox at the top level.
func isCustomFolderName(name string) bool {
	return validFolderName(name) && !isStdFolder(name)
}

// customFolders discovers the user's custom mailboxes — the ".Name" / ".Parent.Child" maildirs
// under the root that are neither standard nor stray directories — as wire names ("Parent/Child"),
// sorted alphabetically (the persisted display order is applied later, in Folders).
func (s *Store) customFolders(user string) []string {
	entries, _ := os.ReadDir(s.userRoot(user))
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := diskNameToFolder(e.Name())
		if name == "" || !isCustomFolderName(name) {
			continue
		}
		// Only count it if it actually looks like a maildir (has cur/).
		if _, err := os.Stat(filepath.Join(s.userRoot(user), e.Name(), "cur")); err != nil {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HasFolder reports whether folder is a usable mailbox for the user: a standard one or an
// existing custom maildir.
func (s *Store) HasFolder(user, folder string) bool {
	if isStdFolder(folder) {
		return true
	}
	if !isCustomFolderName(folder) {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := os.Stat(filepath.Join(folderDir(s.userRoot(user), folder), "cur"))
	return err == nil
}

// CreateFolder makes a new custom mailbox, which may be nested ("Work/Projects"). It rejects
// reserved/invalid names and names that already exist, and "mkdir -p"s any missing ancestor
// folders so the tree is always navigable. The existence check and creation run under the store
// lock so they form one critical section serialized against concurrent rename/delete.
func (s *Store) CreateFolder(user, name string) error {
	name = strings.TrimSpace(name)
	if !isCustomFolderName(name) {
		return ErrInvalidFolder
	}
	if err := s.EnsureUser(user); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ur := s.userRoot(user)
	dir := folderDir(ur, name)
	if _, err := os.Stat(dir); err == nil {
		return ErrFolderExists
	}
	// Materialise any missing ancestors (a std parent like "Sent" already exists; skip it).
	for _, anc := range ancestors(name) {
		if isStdFolder(anc) {
			continue
		}
		if _, err := os.Stat(folderDir(ur, anc)); err != nil {
			if err := ensureMaildir(folderDir(ur, anc)); err != nil {
				return err
			}
			s.appendOrder(user, anc)
		}
	}
	if err := ensureMaildir(dir); err != nil {
		return err
	}
	s.appendOrder(user, name)
	return nil
}

// RenameFolder renames (and/or re-parents) a custom mailbox and its whole subtree. In Maildir++ a
// child ".A.B" is an independent directory from its parent ".A", so every descendant is renamed
// individually. Standard folders cannot be renamed; any new ancestor path is materialised first.
func (s *Store) RenameFolder(user, oldName, newName string) error {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if !isCustomFolderName(oldName) || !isCustomFolderName(newName) {
		return ErrInvalidFolder
	}
	// A folder cannot be moved inside itself (e.g. "A" → "A/B").
	if newName == oldName || strings.HasPrefix(newName, oldName+"/") {
		if newName == oldName {
			return nil
		}
		return ErrInvalidFolder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ur := s.userRoot(user)
	if _, err := os.Stat(folderDir(ur, oldName)); err != nil {
		return os.ErrNotExist
	}
	if _, err := os.Stat(folderDir(ur, newName)); err == nil {
		return ErrFolderExists
	}
	// Plan the rename of the folder and every descendant ".oldName.*" → ".newName.*", validating the
	// whole plan (depth cap, no target collisions) BEFORE any disk side effect — so a rejected move
	// neither half-renames a subtree nor orphans a freshly-created ancestor.
	type step struct{ from, to string }
	var plan []step
	for _, n := range s.subtree(user, oldName) {
		newn := newName + strings.TrimPrefix(n, oldName)
		if !isCustomFolderName(newn) {
			return ErrInvalidFolder
		}
		if _, err := os.Stat(folderDir(ur, newn)); err == nil {
			return ErrFolderExists
		}
		plan = append(plan, step{from: n, to: newn})
	}
	// Only now (the plan is valid) materialise the new parent chain so the moved subtree has a
	// navigable parent node.
	for _, anc := range ancestors(newName) {
		if isStdFolder(anc) {
			continue
		}
		if _, err := os.Stat(folderDir(ur, anc)); err != nil {
			if err := ensureMaildir(folderDir(ur, anc)); err != nil {
				return err
			}
			s.appendOrder(user, anc)
		}
	}
	// Execute, rolling back the renames already done if one fails, so a transient I/O error can
	// never leave a half-renamed subtree on disk.
	var done []step
	for _, st := range plan {
		if err := os.Rename(folderDir(ur, st.from), folderDir(ur, st.to)); err != nil {
			for i := len(done) - 1; i >= 0; i-- {
				_ = os.Rename(folderDir(ur, done[i].to), folderDir(ur, done[i].from))
				s.replaceOrder(user, done[i].to, done[i].from)
			}
			return err
		}
		s.replaceOrder(user, st.from, st.to)
		done = append(done, st)
	}
	return nil
}

// DeleteFolder removes a custom mailbox and its whole subtree. To avoid silent data loss every
// message in the subtree is moved to Trash first (preserving its new/cur placement and flags),
// then the empty folders are removed. Standard folders cannot be deleted.
func (s *Store) DeleteFolder(user, name string) error {
	name = strings.TrimSpace(name)
	if !isCustomFolderName(name) {
		return ErrInvalidFolder
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ur := s.userRoot(user)
	if _, err := os.Stat(folderDir(ur, name)); err != nil {
		return os.ErrNotExist
	}
	trash := folderDir(ur, "Trash")
	if err := ensureMaildir(trash); err != nil {
		return err
	}
	affected := s.subtree(user, name)
	// Relocate every message into Trash first. If any relocation fails, abort BEFORE removing any
	// folder so a message is never destroyed — the contract is "moved to Trash, not lost".
	for _, n := range affected {
		dir := folderDir(ur, n)
		for _, sub := range []string{"new", "cur"} {
			entries, _ := os.ReadDir(filepath.Join(dir, sub))
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if err := os.Rename(filepath.Join(dir, sub, e.Name()), filepath.Join(trash, sub, e.Name())); err != nil {
					return err
				}
			}
		}
	}
	for _, n := range affected {
		if err := os.RemoveAll(folderDir(ur, n)); err != nil {
			return err
		}
		s.removeOrder(user, n)
	}
	return nil
}

// subtree returns the folder itself plus every descendant ("name" and "name/..."), as wire names.
// The caller must hold s.mu. The result is ordered shallowest-first (a valid pre-order).
func (s *Store) subtree(user, name string) []string {
	out := []string{}
	for _, n := range s.customFolders(user) {
		if n == name || strings.HasPrefix(n, name+"/") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func ensureMaildir(dir string) error {
	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// EnsureUser creates the mailbox tree for a user if it does not exist yet.
func (s *Store) EnsureUser(user string) error {
	ur := s.userRoot(user)
	if err := ensureMaildir(ur); err != nil {
		return err
	}
	for _, f := range StdFolders {
		if f == "INBOX" {
			continue
		}
		if err := ensureMaildir(folderDir(ur, f)); err != nil {
			return err
		}
	}
	return nil
}

var deliverySeq uint64

func uniqueName() string {
	host, _ := os.Hostname()
	host = strings.NewReplacer("/", "_", ":", "_", ".", "_").Replace(host)
	if host == "" {
		host = "localhost"
	}
	now := time.Now()
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	seq := atomic.AddUint64(&deliverySeq, 1)
	return fmt.Sprintf("%d.M%dP%dQ%dR%s.%s", now.Unix(), now.Nanosecond()/1000, os.Getpid(), seq, hex.EncodeToString(rnd[:]), host)
}

// Deliver writes data into the user's folder. seen=false drops it in new/ (unread);
// seen=true stores it in cur/ already-Seen (used for the sender's Sent copy). Returns the
// message id (the maildir base name).
func (s *Store) Deliver(user, folder string, data []byte, seen bool) (string, error) {
	if err := s.EnsureUser(user); err != nil {
		return "", err
	}
	fdir := folderDir(s.userRoot(user), folder)
	if err := ensureMaildir(fdir); err != nil {
		return "", err
	}
	name := uniqueName()
	tmp := filepath.Join(fdir, "tmp", name)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	dst := filepath.Join(fdir, "new", name)
	if seen {
		dst = filepath.Join(fdir, "cur", name+":2,S")
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return name, nil
}

// baseAndFlags splits a maildir filename into its unique base and flag letters.
func baseAndFlags(name string) (string, string) {
	if i := strings.Index(name, ":2,"); i >= 0 {
		return name[:i], name[i+3:]
	}
	return name, ""
}

// find locates a message by id within a folder, returning its full path and flags.
func find(fdir, id string) (path, flags string, ok bool) {
	np := filepath.Join(fdir, "new", id)
	if _, err := os.Stat(np); err == nil {
		return np, "", true
	}
	curDir := filepath.Join(fdir, "cur")
	entries, _ := os.ReadDir(curDir)
	for _, e := range entries {
		base, fl := baseAndFlags(e.Name())
		if base == id {
			return filepath.Join(curDir, e.Name()), fl, true
		}
	}
	return "", "", false
}

// List returns the messages in a folder, newest first.
func (s *Store) List(user, folder string) ([]Message, error) {
	if err := s.EnsureUser(user); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	fdir := folderDir(s.userRoot(user), folder)
	var out []Message
	collect := func(sub string, inNew bool) {
		dir := filepath.Join(fdir, sub)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			base, fl := baseAndFlags(e.Name())
			full := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				continue
			}
			raw, err := os.ReadFile(full)
			if err != nil {
				continue
			}
			h, _ := message.Summary(raw)
			out = append(out, Message{
				ID: base, Folder: folder,
				From: h.From, To: h.To, Subject: h.Subject, Date: h.Date,
				MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
				Seen:           !inNew && strings.Contains(fl, "S"),
				Flagged:        strings.Contains(fl, "F"),
				Answered:       strings.Contains(fl, "R"),
				HasAttachments: h.HasAttachments,
				Size:           info.Size(),
			})
		}
	}
	collect("new", true)
	collect("cur", false)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out, nil
}

// ReadRaw returns the full bytes of a message.
func (s *Store) ReadRaw(user, folder, id string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, _, ok := find(folderDir(s.userRoot(user), folder), id)
	if !ok {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(path)
}

// Get returns a message's metadata (with flags) and raw bytes in a single lookup.
func (s *Store) Get(user, folder, id string) (Message, []byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path, fl, ok := find(folderDir(s.userRoot(user), folder), id)
	if !ok {
		return Message{}, nil, os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Message{}, nil, err
	}
	h, _ := message.Summary(raw)
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	m := Message{
		ID: id, Folder: folder,
		From: h.From, To: h.To, Subject: h.Subject, Date: h.Date,
		MessageID: h.MessageID, InReplyTo: h.InReplyTo, References: h.References,
		Seen: strings.Contains(fl, "S"), Flagged: strings.Contains(fl, "F"), Answered: strings.Contains(fl, "R"),
		HasAttachments: h.HasAttachments,
		Size:           size,
	}
	return m, raw, nil
}

// Flags is a partial flag update; nil fields are left unchanged.
type Flags struct {
	Seen     *bool
	Flagged  *bool
	Answered *bool
}

// SetFlags applies a partial flag change, moving the message into cur/ if needed.
func (s *Store) SetFlags(user, folder, id string, fl Flags) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fdir := folderDir(s.userRoot(user), folder)
	path, cur, ok := find(fdir, id)
	if !ok {
		return os.ErrNotExist
	}
	set := map[string]bool{}
	for _, r := range cur {
		set[string(r)] = true
	}
	apply := func(letter string, v *bool) {
		if v == nil {
			return
		}
		if *v {
			set[letter] = true
		} else {
			delete(set, letter)
		}
	}
	apply("S", fl.Seen)
	apply("F", fl.Flagged)
	apply("R", fl.Answered)
	newPath := filepath.Join(fdir, "cur", id+":2,"+joinFlags(set))
	if path == newPath {
		return nil
	}
	return os.Rename(path, newPath)
}

// Move relocates a message to another folder, preserving its flags.
func (s *Store) Move(user, from, to, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ur := s.userRoot(user)
	src, fl, ok := find(folderDir(ur, from), id)
	if !ok {
		return os.ErrNotExist
	}
	dstDir := folderDir(ur, to)
	if err := ensureMaildir(dstDir); err != nil {
		return err
	}
	var dst string
	if fl != "" {
		dst = filepath.Join(dstDir, "cur", id+":2,"+fl)
	} else {
		dst = filepath.Join(dstDir, "new", id)
	}
	return os.Rename(src, dst)
}

// Delete moves a message to Trash, or removes it permanently if already in Trash.
func (s *Store) Delete(user, folder, id string) error {
	if folder == "Trash" {
		s.mu.Lock()
		defer s.mu.Unlock()
		path, _, ok := find(folderDir(s.userRoot(user), folder), id)
		if !ok {
			return os.ErrNotExist
		}
		return os.Remove(path)
	}
	return s.Move(user, folder, "Trash", id)
}

// Folders returns the standard mailboxes followed by the user's custom folders, each with its
// counts. Standard folders keep their fixed canonical order; custom ones follow the user's
// persisted display order (newly-discovered ones, with no stored position, sort alphabetically
// after the ordered set). The UI derives the tree from the "/"-delimited names.
func (s *Store) Folders(user string) ([]FolderInfo, error) {
	if err := s.EnsureUser(user); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ur := s.userRoot(user)
	custom := applyOrder(s.customFolders(user), s.readOrder(user))
	out := make([]FolderInfo, 0, len(StdFolders)+len(custom))
	for _, f := range StdFolders {
		total, unread := folderCounts(folderDir(ur, f))
		out = append(out, FolderInfo{Name: f, Total: total, Unread: unread})
	}
	for _, f := range custom {
		total, unread := folderCounts(folderDir(ur, f))
		out = append(out, FolderInfo{Name: f, Total: total, Unread: unread, Custom: true})
	}
	return out, nil
}

// SetFolderOrder persists the user's chosen display order for custom folders. Unknown names are
// dropped and any existing custom folder missing from the request is appended (alphabetically), so
// the stored order is always a clean permutation of what actually exists.
func (s *Store) SetFolderOrder(user string, order []string) error {
	if err := s.EnsureUser(user); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := map[string]bool{}
	for _, n := range s.customFolders(user) {
		existing[n] = true
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(existing))
	for _, n := range order {
		n = strings.TrimSpace(n)
		if existing[n] && !seen[n] {
			clean = append(clean, n)
			seen[n] = true
		}
	}
	missing := make([]string, 0)
	for n := range existing {
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	clean = append(clean, missing...)
	return s.writeOrder(user, clean)
}

// applyOrder sorts names so that those present in order come first (in order's sequence) and the
// rest follow alphabetically. Stable and pure.
func applyOrder(names, order []string) []string {
	pos := make(map[string]int, len(order))
	for i, n := range order {
		if _, ok := pos[n]; !ok {
			pos[n] = i
		}
	}
	out := append([]string(nil), names...)
	sort.SliceStable(out, func(i, j int) bool {
		pi, oki := pos[out[i]]
		pj, okj := pos[out[j]]
		if oki && okj {
			return pi < pj
		}
		if oki != okj {
			return oki
		}
		return out[i] < out[j]
	})
	return out
}

// ── per-user folder order persistence (atomic JSON, caller holds s.mu) ────────────────

type folderOrder struct {
	Order []string `json:"order"`
}

func (s *Store) orderPath(user string) string { return filepath.Join(s.userDir(user), folderOrderFile) }

func (s *Store) readOrder(user string) []string {
	b, err := os.ReadFile(s.orderPath(user))
	if err != nil {
		return nil
	}
	var fo folderOrder
	if json.Unmarshal(b, &fo) != nil {
		return nil
	}
	return fo.Order
}

func (s *Store) writeOrder(user string, order []string) error {
	b, err := json.MarshalIndent(folderOrder{Order: order}, "", "  ")
	if err != nil {
		return err
	}
	path := s.orderPath(user)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// appendOrder adds name to the end of the stored order if absent. Best-effort (order is a display
// hint, never a source of truth): a write failure must not fail the folder mutation.
func (s *Store) appendOrder(user, name string) {
	order := s.readOrder(user)
	for _, n := range order {
		if n == name {
			return
		}
	}
	_ = s.writeOrder(user, append(order, name))
}

// replaceOrder swaps an old name for a new one in the stored order (used by rename/re-parent).
func (s *Store) replaceOrder(user, oldName, newName string) {
	order := s.readOrder(user)
	changed := false
	for i, n := range order {
		if n == oldName {
			order[i] = newName
			changed = true
		}
	}
	if changed {
		_ = s.writeOrder(user, order)
	}
}

// removeOrder drops a name from the stored order (used by delete).
func (s *Store) removeOrder(user, name string) {
	order := s.readOrder(user)
	out := order[:0]
	dropped := false
	for _, n := range order {
		if n == name {
			dropped = true
			continue
		}
		out = append(out, n)
	}
	if dropped {
		_ = s.writeOrder(user, out)
	}
}

// folderCounts returns the total and unread message counts for a maildir folder.
func folderCounts(fdir string) (total, unread int) {
	newNames, _ := os.ReadDir(filepath.Join(fdir, "new"))
	curNames, _ := os.ReadDir(filepath.Join(fdir, "cur"))
	unread = len(newNames)
	for _, e := range curNames {
		if _, fl := baseAndFlags(e.Name()); !strings.Contains(fl, "S") {
			unread++
		}
	}
	return len(newNames) + len(curNames), unread
}

// joinFlags returns the maildir info flags in canonical (ascending) order.
func joinFlags(set map[string]bool) string {
	letters := make([]string, 0, len(set))
	for l := range set {
		letters = append(letters, l)
	}
	sort.Strings(letters)
	return strings.Join(letters, "")
}
