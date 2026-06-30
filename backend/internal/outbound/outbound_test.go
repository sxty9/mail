package outbound

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
