package registry

import (
	"testing"

	"mail/internal/instance"
)

// newTestRegistry builds a registry over a temp dir with a fixed primary domain.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	t.Setenv("HOLISTIC_MAIL_DOMAIN", "primary.test")
	return New(t.TempDir(), instance.New())
}

func TestResolveDefaultScheme(t *testing.T) {
	r := newTestRegistry(t)

	// <user>@<primary> resolves to the user via the default scheme.
	if u, ok := r.Resolve("nanu@primary.test"); !ok || u != "nanu" {
		t.Errorf("Resolve(nanu@primary.test) = %q,%v; want nanu,true", u, ok)
	}
	// Case-insensitive on both localpart and domain.
	if u, ok := r.Resolve("NANU@Primary.Test"); !ok || u != "nanu" {
		t.Errorf("Resolve mixed-case = %q,%v; want nanu,true", u, ok)
	}
	// An unserved domain is not local.
	if _, ok := r.Resolve("nanu@elsewhere.test"); ok {
		t.Error("Resolve on unserved domain should be non-local")
	}
	// A malformed localpart on a served domain is not local.
	if _, ok := r.Resolve("not a user@primary.test"); ok {
		t.Error("Resolve with bad localpart should be non-local")
	}
}

func TestAddDomainAndResolveAcrossDomains(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.AddDomain("second.test", "ses"); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if !r.Serves("second.test") {
		t.Error("second.test should be served after AddDomain")
	}
	// The same mailbox is reachable on the new domain via the default scheme.
	if u, ok := r.Resolve("nanu@second.test"); !ok || u != "nanu" {
		t.Errorf("Resolve(nanu@second.test) = %q,%v; want nanu,true", u, ok)
	}
	// Domains() lists the primary first, then registered ones.
	ds := r.Domains()
	if len(ds) != 2 || !ds[0].Primary || ds[0].Name != "primary.test" || ds[1].Name != "second.test" {
		t.Fatalf("Domains() = %+v", ds)
	}
	// A bad domain is rejected.
	if _, err := r.AddDomain("not_a_domain", ""); err != ErrInvalidDomain {
		t.Errorf("AddDomain(bad) err = %v, want ErrInvalidDomain", err)
	}
	// The primary cannot be removed.
	if err := r.RemoveDomain("primary.test"); err != ErrPrimaryDomain {
		t.Errorf("RemoveDomain(primary) err = %v, want ErrPrimaryDomain", err)
	}
}

func TestAliasPrecedenceAndPersistence(t *testing.T) {
	r := newTestRegistry(t)
	// "admin" is itself a valid username, so the default scheme alone would map admin→admin.
	// The explicit alias must take precedence and route it to nanu instead.
	if _, err := r.AddAlias("admin@primary.test", "nanu"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	if u, ok := r.Resolve("admin@primary.test"); !ok || u != "nanu" {
		t.Errorf("Resolve(admin@) = %q,%v; want nanu,true (alias beats default scheme)", u, ok)
	}
	// Alias on an unserved domain is rejected.
	if _, err := r.AddAlias("info@elsewhere.test", "nanu"); err != ErrInvalidAlias {
		t.Errorf("AddAlias(unserved) err = %v, want ErrInvalidAlias", err)
	}

	// Reload from disk: the alias must survive (atomic persistence).
	r2 := New(r.dir, r.inst)
	if u, ok := r2.Resolve("admin@primary.test"); !ok || u != "nanu" {
		t.Errorf("after reload Resolve(admin@) = %q,%v; want nanu,true", u, ok)
	}

	// Removing the alias restores the default scheme (admin@ → user "admin"), it does not make
	// the address non-local.
	if err := r2.RemoveAlias("admin@primary.test"); err != nil {
		t.Fatalf("RemoveAlias: %v", err)
	}
	if u, ok := r2.Resolve("admin@primary.test"); !ok || u != "admin" {
		t.Errorf("after RemoveAlias Resolve(admin@) = %q,%v; want admin,true (default scheme)", u, ok)
	}
}

func TestAddressesAndOwns(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.AddDomain("second.test", ""); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if _, err := r.AddAlias("hello@second.test", "nanu"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	addrs := r.Addresses("nanu")
	// Primary first.
	if len(addrs) == 0 || addrs[0] != "nanu@primary.test" {
		t.Fatalf("Addresses[0] = %v, want nanu@primary.test (got %v)", addrs[0], addrs)
	}
	want := map[string]bool{"nanu@primary.test": true, "nanu@second.test": true, "hello@second.test": true}
	if len(addrs) != len(want) {
		t.Fatalf("Addresses = %v, want %d entries", addrs, len(want))
	}
	for _, a := range addrs {
		if !want[a] {
			t.Errorf("unexpected address %q in %v", a, addrs)
		}
	}
	// Owns is the send-as gate.
	if !r.Owns("nanu", "hello@second.test") {
		t.Error("nanu should own hello@second.test")
	}
	if r.Owns("nanu", "someoneelse@primary.test") {
		t.Error("nanu must not own someoneelse@primary.test")
	}
}

// TestRemoveDomainDropsAliases verifies aliases on a removed domain are cleaned up.
func TestRemoveDomainDropsAliases(t *testing.T) {
	r := newTestRegistry(t)
	if _, err := r.AddDomain("second.test", ""); err != nil {
		t.Fatalf("AddDomain: %v", err)
	}
	if _, err := r.AddAlias("x@second.test", "nanu"); err != nil {
		t.Fatalf("AddAlias: %v", err)
	}
	if err := r.RemoveDomain("second.test"); err != nil {
		t.Fatalf("RemoveDomain: %v", err)
	}
	if _, ok := r.Resolve("x@second.test"); ok {
		t.Error("alias on removed domain should no longer resolve")
	}
	if r.Serves("second.test") {
		t.Error("second.test should no longer be served")
	}
}
