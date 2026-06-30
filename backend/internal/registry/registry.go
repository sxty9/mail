// Package registry is maild's identity map: which mail domains this instance serves and which
// explicit alias addresses route to which local mailbox. It is the single answer to "is this
// address local, and whose mailbox is it?" and to "which addresses may this user send from?".
//
// The model is deliberately one mailbox / many addresses: a user's Maildir is reachable at
// <user>@<every served domain> by default, plus any number of explicit aliases (info@…, a
// vanity name, an address on another owned domain). It is NOT a second mailbox — every alias
// for a user lands in that one user's Maildir.
//
// State is two small JSON files under <dataRoot>/registry, written atomically (temp+rename),
// matching holistic's "files, no database" convention. The instance primary domain stays owned
// by the dashboard (instance.json); the registry only ADDS domains/aliases on top of it.
package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"mail/internal/instance"
)

// Validation patterns. usernameRE mirrors the LDA's local-user rule (a provisionable Linux
// account); domainRE/localpartRE keep alias + domain inputs safe and canonical (lowercase).
var (
	usernameRE  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	domainRE    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	localpartRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,63}$`)
)

// Registry errors.
var (
	ErrInvalidDomain = errors.New("invalid domain name")
	ErrInvalidAlias  = errors.New("invalid alias address")
	ErrInvalidUser   = errors.New("invalid target user")
	ErrPrimaryDomain = errors.New("cannot remove the primary domain")
	ErrNotFound      = errors.New("not found")
)

// Domain is a served mail domain. Primary marks the dashboard's instance domain (always served,
// never removable here). Status is "verified" or "pending"; Provider records who does external
// transport/DKIM for it (e.g. "ses"), for the admin/automation layer.
type Domain struct {
	Name     string    `json:"name"`
	Primary  bool      `json:"primary"`
	Status   string    `json:"status"`
	Provider string    `json:"provider,omitempty"`
	AddedAt  time.Time `json:"addedAt"`
}

// Alias maps one explicit address to the local mailbox owner it delivers to.
type Alias struct {
	Address string `json:"address"`
	User    string `json:"user"`
}

// Registry holds the in-memory view plus the on-disk path. All public methods are safe for
// concurrent use.
type Registry struct {
	dir  string
	inst *instance.Resolver

	mu      sync.RWMutex
	domains map[string]Domain // keyed by lowercase name (excludes the instance primary)
	aliases map[string]string // lowercase address -> username
}

// New loads the registry from <dir> (creating nothing until first write). Missing files are
// treated as empty, so a single-domain instance with no registry works unchanged.
func New(dir string, inst *instance.Resolver) *Registry {
	r := &Registry{
		dir:     dir,
		inst:    inst,
		domains: map[string]Domain{},
		aliases: map[string]string{},
	}
	r.load()
	return r
}

func (r *Registry) domainsPath() string { return filepath.Join(r.dir, "domains.json") }
func (r *Registry) aliasesPath() string { return filepath.Join(r.dir, "aliases.json") }

func (r *Registry) load() {
	var ds []Domain
	if readJSON(r.domainsPath(), &ds) {
		for _, d := range ds {
			d.Name = strings.ToLower(strings.TrimSpace(d.Name))
			if d.Name != "" && !d.Primary {
				r.domains[d.Name] = d
			}
		}
	}
	var as []Alias
	if readJSON(r.aliasesPath(), &as) {
		for _, a := range as {
			addr := strings.ToLower(strings.TrimSpace(a.Address))
			u := strings.ToLower(strings.TrimSpace(a.User))
			if addr != "" && u != "" {
				r.aliases[addr] = u
			}
		}
	}
}

// primaryDomain is the dashboard's instance domain, lowercased ("" if none configured yet).
func (r *Registry) primaryDomain() string {
	return strings.ToLower(strings.TrimSpace(r.inst.MailDomain()))
}

// servedDomains is the set of every domain this instance accepts mail for: the primary plus the
// registered ones. Caller must NOT hold r.mu.
func (r *Registry) servedDomains() map[string]bool {
	set := map[string]bool{}
	if p := r.primaryDomain(); p != "" {
		set[p] = true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for name := range r.domains {
		set[name] = true
	}
	return set
}

// Serves reports whether domain is one this instance accepts mail for.
func (r *Registry) Serves(domain string) bool {
	return r.servedDomains()[strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")]
}

// Resolve maps a recipient address to the local mailbox owner. Precedence:
//  1. an explicit alias wins outright;
//  2. otherwise the default scheme applies — a served domain with a well-formed username
//     localpart resolves to <localpart>.
//
// ok=false means "not local" (the caller spools it externally). Resolve does not check that the
// resolved user actually exists as an OS account — that is the delivery layer's job.
func (r *Registry) Resolve(addr string) (user string, ok bool) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", false
	}
	// Normalise an FQDN trailing dot ("nanu@primary.test.") so it resolves identically to the
	// bare form — otherwise local mail would be mis-routed externally.
	local, domain := addr[:at], strings.TrimSuffix(addr[at+1:], ".")
	canonical := local + "@" + domain

	r.mu.RLock()
	u, aliased := r.aliases[canonical]
	r.mu.RUnlock()
	if aliased {
		return u, true
	}

	if !r.servedDomains()[domain] {
		return "", false
	}
	if !usernameRE.MatchString(local) {
		return "", false
	}
	return local, true
}

// Addresses returns every address a user owns — and may therefore send from: the default
// <user>@<served domain> for each served domain, plus every alias that routes to the user. The
// user's primary address (<user>@<primary domain>) sorts first; the rest are alphabetical.
func (r *Registry) Addresses(user string) []string {
	user = strings.ToLower(strings.TrimSpace(user))
	if !usernameRE.MatchString(user) {
		return nil
	}
	primary := ""
	if p := r.primaryDomain(); p != "" {
		primary = user + "@" + p
	}
	seen := map[string]bool{}
	var rest []string
	add := func(a string) {
		if a == "" || a == primary || seen[a] {
			return
		}
		seen[a] = true
		rest = append(rest, a)
	}
	for d := range r.servedDomains() {
		add(user + "@" + d)
	}
	r.mu.RLock()
	for addr, u := range r.aliases {
		if u == user {
			add(addr)
		}
	}
	r.mu.RUnlock()
	sort.Strings(rest)
	out := make([]string, 0, len(rest)+1)
	if primary != "" {
		out = append(out, primary)
	}
	return append(out, rest...)
}

// Owns reports whether addr is an address the user is allowed to send from.
func (r *Registry) Owns(user, addr string) bool {
	addr = strings.ToLower(strings.TrimSpace(addr))
	for _, a := range r.Addresses(user) {
		if a == addr {
			return true
		}
	}
	return false
}

// DefaultAddress is the user's primary address (<user>@<primary domain>), or just the username
// if no domain is configured yet. Mirrors instance.Address but lives here for symmetry.
func (r *Registry) DefaultAddress(user string) string {
	return r.inst.Address(user)
}

// Domains returns every served domain: the primary first, then registered ones alphabetically.
func (r *Registry) Domains() []Domain {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Domain, 0, len(r.domains)+1)
	if p := r.primaryDomain(); p != "" {
		out = append(out, Domain{Name: p, Primary: true, Status: "verified"})
	}
	names := make([]string, 0, len(r.domains))
	for name := range r.domains {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, r.domains[name])
	}
	return out
}

// AddDomain registers a new served domain (status defaults to "pending"). Re-adding the primary
// or an existing domain is a no-op success when the metadata matches; otherwise it updates it.
func (r *Registry) AddDomain(name, provider string) (Domain, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !domainRE.MatchString(name) {
		return Domain{}, ErrInvalidDomain
	}
	if name == r.primaryDomain() {
		return Domain{Name: name, Primary: true, Status: "verified"}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, existed := r.domains[name]
	d := prev
	if !existed {
		d = Domain{Name: name, Status: "pending", Provider: provider, AddedAt: time.Now().UTC()}
	} else if provider != "" {
		d.Provider = provider
	}
	r.domains[name] = d
	if err := r.persistDomains(); err != nil {
		r.restoreDomain(name, prev, existed) // keep memory matching the failed-to-persist disk
		return Domain{}, err
	}
	return d, nil
}

// SetDomainStatus updates a registered domain's verification status (e.g. "pending"→"verified").
func (r *Registry) SetDomainStatus(name, status string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.domains[name]
	if !ok {
		return ErrNotFound
	}
	d := prev
	d.Status = status
	r.domains[name] = d
	if err := r.persistDomains(); err != nil {
		r.domains[name] = prev
		return err
	}
	return nil
}

// RemoveDomain unregisters a domain and drops every alias on it. The primary cannot be removed.
func (r *Registry) RemoveDomain(name string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == r.primaryDomain() {
		return ErrPrimaryDomain
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.domains[name]; !ok {
		return ErrNotFound
	}
	// Removing a domain touches BOTH files (the domain and its aliases). Snapshot first so that a
	// partial write failure restores memory wholesale rather than leaving a removed domain whose
	// aliases still resolve after a reload.
	savedDomains, savedAliases := cloneDomains(r.domains), cloneStrMap(r.aliases)
	delete(r.domains, name)
	for addr := range r.aliases {
		if strings.HasSuffix(addr, "@"+name) {
			delete(r.aliases, addr)
		}
	}
	if err := r.persistDomains(); err != nil {
		r.domains, r.aliases = savedDomains, savedAliases
		return err
	}
	if err := r.persistAliases(); err != nil {
		r.domains, r.aliases = savedDomains, savedAliases
		_ = r.persistDomains() // best effort: re-sync domains.json with the restored memory
		return err
	}
	return nil
}

// AddAlias points an explicit address at a local mailbox owner. The address must sit on a served
// domain and the target must be a well-formed username.
func (r *Registry) AddAlias(addr, user string) (Alias, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	user = strings.ToLower(strings.TrimSpace(user))
	at := strings.LastIndex(addr, "@")
	if at <= 0 {
		return Alias{}, ErrInvalidAlias
	}
	local, domain := addr[:at], addr[at+1:]
	if !localpartRE.MatchString(local) || !r.Serves(domain) {
		return Alias{}, ErrInvalidAlias
	}
	if !usernameRE.MatchString(user) {
		return Alias{}, ErrInvalidUser
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, existed := r.aliases[addr]
	r.aliases[addr] = user
	if err := r.persistAliases(); err != nil {
		if existed {
			r.aliases[addr] = prev
		} else {
			delete(r.aliases, addr)
		}
		return Alias{}, err
	}
	return Alias{Address: addr, User: user}, nil
}

// RemoveAlias deletes an explicit alias.
func (r *Registry) RemoveAlias(addr string) error {
	addr = strings.ToLower(strings.TrimSpace(addr))
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, ok := r.aliases[addr]
	if !ok {
		return ErrNotFound
	}
	delete(r.aliases, addr)
	if err := r.persistAliases(); err != nil {
		r.aliases[addr] = prev
		return err
	}
	return nil
}

// Aliases returns every explicit alias, sorted by address.
func (r *Registry) Aliases() []Alias {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedAliases(r.aliases, "")
}

// AliasesFor returns the explicit aliases routing to one user, sorted by address.
func (r *Registry) AliasesFor(user string) []Alias {
	user = strings.ToLower(strings.TrimSpace(user))
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedAliases(r.aliases, user)
}

func sortedAliases(m map[string]string, onlyUser string) []Alias {
	out := make([]Alias, 0, len(m))
	for addr, u := range m {
		if onlyUser != "" && u != onlyUser {
			continue
		}
		out = append(out, Alias{Address: addr, User: u})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

// persistDomains/persistAliases write the current state atomically. Callers hold r.mu.
func (r *Registry) persistDomains() error {
	list := make([]Domain, 0, len(r.domains))
	for _, d := range r.domains {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return writeJSONAtomic(r.dir, r.domainsPath(), list)
}

func (r *Registry) persistAliases() error {
	return writeJSONAtomic(r.dir, r.aliasesPath(), sortedAliases(r.aliases, ""))
}

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// writeJSONAtomic encodes v to path via a temp file + rename, so a reader never sees a partial
// file and a crash never corrupts the store.
func writeJSONAtomic(dir, path string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName) // don't leak the temp file on a failed rename
		return err
	}
	// fsync the directory so the rename entry itself is durable across a crash.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// restoreDomain reverts a domain entry after a failed persist (caller holds r.mu).
func (r *Registry) restoreDomain(name string, prev Domain, existed bool) {
	if existed {
		r.domains[name] = prev
	} else {
		delete(r.domains, name)
	}
}

func cloneDomains(m map[string]Domain) map[string]Domain {
	out := make(map[string]Domain, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneStrMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
