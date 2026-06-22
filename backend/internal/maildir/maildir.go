// Package maildir is maild's message store: a per-user Maildir++ tree that IS the single
// source of truth for message bytes and flags (no database, matching the holistic
// "files + atomic writes" convention). A mailbox lives at <root>/<user>/Maildir with
// INBOX = the top dir and Sent/Drafts/Trash as ".Name" submaildirs. Delivery is the
// standard tmp→new (or tmp→cur) rename, which is atomic on a single filesystem.
package maildir

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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

// Store is a set of Maildirs rooted at a single directory.
type Store struct {
	root string
	mu   sync.Mutex // serializes flag/move/delete renames (low-traffic, single instance)
}

// New returns a store whose mailboxes live under root (e.g. /var/lib/mail/mailboxes).
func New(root string) *Store { return &Store{root: root} }

// Message is one message's metadata for a list view (no body).
type Message struct {
	ID         string    `json:"id"` // maildir base name (unique part, no :2, flags)
	Folder     string    `json:"folder"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Subject    string    `json:"subject"`
	Date       time.Time `json:"date"`
	MessageID  string    `json:"messageId"`
	InReplyTo  string    `json:"inReplyTo"`
	References []string  `json:"references"`
	Seen       bool      `json:"seen"`
	Flagged    bool      `json:"flagged"`
	Answered   bool      `json:"answered"`
	Size       int64     `json:"size"`
}

// FolderInfo is a mailbox plus its message counts.
type FolderInfo struct {
	Name   string `json:"name"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
}

func (s *Store) userRoot(user string) string { return filepath.Join(s.root, user, "Maildir") }

func folderDir(userRoot, folder string) string {
	if folder == "" || folder == "INBOX" {
		return userRoot
	}
	return filepath.Join(userRoot, "."+folder)
}

// ValidFolder reports whether folder is one we manage (guards path traversal).
func ValidFolder(folder string) bool {
	for _, f := range StdFolders {
		if f == folder {
			return true
		}
	}
	return false
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
				Seen:     !inNew && strings.Contains(fl, "S"),
				Flagged:  strings.Contains(fl, "F"),
				Answered: strings.Contains(fl, "R"),
				Size:     info.Size(),
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
	path, _, ok := find(folderDir(s.userRoot(user), folder), id)
	if !ok {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(path)
}

// Get returns a message's metadata (with flags) and raw bytes in a single lookup.
func (s *Store) Get(user, folder, id string) (Message, []byte, error) {
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
		Size: size,
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

// Folders returns the standard mailboxes with their counts.
func (s *Store) Folders(user string) ([]FolderInfo, error) {
	if err := s.EnsureUser(user); err != nil {
		return nil, err
	}
	ur := s.userRoot(user)
	out := make([]FolderInfo, 0, len(StdFolders))
	for _, f := range StdFolders {
		fdir := folderDir(ur, f)
		newNames, _ := os.ReadDir(filepath.Join(fdir, "new"))
		curNames, _ := os.ReadDir(filepath.Join(fdir, "cur"))
		unread := len(newNames)
		for _, e := range curNames {
			_, fl := baseAndFlags(e.Name())
			if !strings.Contains(fl, "S") {
				unread++
			}
		}
		out = append(out, FolderInfo{Name: f, Total: len(newNames) + len(curNames), Unread: unread})
	}
	return out, nil
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
