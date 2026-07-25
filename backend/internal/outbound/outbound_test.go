package outbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFlushSuccessRemovesJob: a 2xx from the edge removes the spooled job (no resend).
func TestFlushSuccessRemovesJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	q := New(t.TempDir(), srv.URL, "s")
	if _, err := q.Enqueue("a@b.test", []string{"c@d.test"}, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	q.flush(context.Background())
	if q.Pending() != 0 {
		t.Fatalf("pending = %d, want 0 after a successful flush", q.Pending())
	}
}

// TestFlushCapDeadLetters is the regression for the infinite re-delivery loop: a persistently
// failing edge must stop being retried after maxAttempts, with the job dead-lettered (not looped).
func TestFlushCapDeadLetters(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := t.TempDir()
	q := New(dir, srv.URL, "s")
	id, err := q.Enqueue("a@b.test", []string{"c@d.test"}, []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxAttempts+5; i++ {
		q.flush(context.Background())
	}
	if hits > maxAttempts {
		t.Errorf("edge was hit %d times, want at most %d (no infinite retry)", hits, maxAttempts)
	}
	if q.Pending() != 0 {
		t.Fatalf("pending = %d, want 0 after dead-letter", q.Pending())
	}
	if _, err := os.Stat(filepath.Join(dir, "failed", id+".json")); err != nil {
		t.Errorf("job was not dead-lettered to failed/: %v", err)
	}
}

// TestEnqueueConcurrentUniqueIDs guards the spool against id-collision data loss: N concurrent
// Enqueue calls must persist N distinct jobs. A purely time-based id can collide when two
// goroutines land in the same nanosecond, and the second writeFileAtomic would clobber the first —
// silently dropping an outbound message.
func TestEnqueueConcurrentUniqueIDs(t *testing.T) {
	q := New(t.TempDir(), "", "") // no edge → jobs just spool, nothing is delivered
	const n = 200
	ids := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id, err := q.Enqueue("a@b.test", []string{"c@d.test"}, []byte("hi"))
			if err != nil {
				t.Errorf("enqueue: %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if seen[id] {
			t.Errorf("duplicate id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct ids, want %d", len(seen), n)
	}
	if q.Pending() != n {
		t.Errorf("pending = %d, want %d (a collision would clobber and drop a job)", q.Pending(), n)
	}
}

// TestDeliverReapsPhantomJob is the self-heal for a marker that outlived its body: a <id>.json with
// no <id>.eml must be reaped, not retried forever, and the edge must never be contacted for it.
func TestDeliverReapsPhantomJob(t *testing.T) {
	dir := t.TempDir()
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q := New(dir, srv.URL, "s")
	meta, _ := json.MarshalIndent(Job{ID: "ghost", From: "a@b.test", To: []string{"c@d.test"}, QueuedAt: time.Now().UTC()}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "ghost.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	q.flush(context.Background())
	if hit {
		t.Error("edge was contacted for a body-less job")
	}
	if q.Pending() != 0 {
		t.Errorf("pending = %d, want 0 (phantom job should be reaped)", q.Pending())
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost.json")); !os.IsNotExist(err) {
		t.Errorf("phantom marker still present: %v", err)
	}
}

// TestDeliverReapsCorruptJob: a <id>.json that isn't valid JSON is discarded (reaped) rather than
// retried on every flush forever.
func TestDeliverReapsCorruptJob(t *testing.T) {
	dir := t.TempDir()
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	q := New(dir, srv.URL, "s")
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.eml"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	q.flush(context.Background())
	if hit {
		t.Error("edge was contacted for a corrupt job")
	}
	if q.Pending() != 0 {
		t.Errorf("pending = %d, want 0 (corrupt job should be reaped)", q.Pending())
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.eml")); !os.IsNotExist(err) {
		t.Errorf("corrupt job's .eml still present: %v", err)
	}
}
