// Package profile reads the Holistic account profile for a user (first/last name, nickname)
// from the dashboard's profile store — the single source of truth for identity. maild runs as
// user `mail` in group `holistic`, so it can read /var/lib/holistic/profiles/<user>.json the
// same way internal/instance reads instance.json. Used to put a real display name on the From
// header ("First Last <user@domain>") instead of a bare address. Read-only; never writes.
package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// nameRE guards against path traversal: a username must look like a Linux account name.
var nameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// UserProfile mirrors the app-managed fields the dashboard persists per user.
type UserProfile struct {
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
	Nickname  string `json:"nickname"`
}

// DisplayName returns "First Last", falling back to the nickname, else "".
func (p UserProfile) DisplayName() string {
	if full := strings.TrimSpace(p.FirstName + " " + p.LastName); full != "" {
		return full
	}
	return strings.TrimSpace(p.Nickname)
}

// Resolver reads (and briefly caches) profiles from the dashboard's store.
type Resolver struct {
	root string

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	p  UserProfile
	at time.Time
}

// New honours HOLISTIC_PROFILES (default /var/lib/holistic/profiles).
func New() *Resolver {
	root := strings.TrimSpace(os.Getenv("HOLISTIC_PROFILES"))
	if root == "" {
		root = "/var/lib/holistic/profiles"
	}
	return &Resolver{root: root, cache: make(map[string]entry)}
}

// Load returns the profile for a user, or a zero profile if absent/unreadable (degrades
// gracefully, exactly like the dashboard). Cached for 30s per user.
func (r *Resolver) Load(username string) UserProfile {
	username = strings.ToLower(strings.TrimSpace(username))
	if !nameRE.MatchString(username) {
		return UserProfile{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.cache[username]; ok && time.Since(e.at) < 30*time.Second {
		return e.p
	}
	var p UserProfile
	if b, err := os.ReadFile(filepath.Join(r.root, username+".json")); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	r.cache[username] = entry{p: p, at: time.Now()}
	return p
}
