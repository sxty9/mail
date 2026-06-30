// Package outbound is maild's spool for messages bound for the public internet. maild does
// NOT talk SMTP itself — per the architecture, the sxgate "mail edge" owns DKIM signing and
// the smarthost relay. This queue persists each message to disk (atomic write) and hands it
// to the edge over an authenticated loopback HTTP call; until the edge is configured
// (MAILD_EDGE_URL unset) jobs simply wait, so internal mail works with zero edge.
package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxAttempts caps outbound delivery retries. After this many consecutive failures a job is
// dead-lettered (moved to a "failed" subdir) instead of being retried forever — the backstop
// against an infinite re-delivery loop (e.g. a slow edge that relays successfully but times out
// our client, so every retry re-delivers the same mail).
const maxAttempts = 5

// Queue is a disk-backed outbound spool with a background flush loop.
type Queue struct {
	dir     string
	edgeURL string
	secret  string
	client  *http.Client
	kick    chan struct{}
}

// Job is the metadata persisted alongside each spooled message.
type Job struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	To       []string  `json:"to"`
	QueuedAt time.Time `json:"queuedAt"`
	Attempts int       `json:"attempts"`
}

// New creates the spool. edgeURL is the sxgate edge submission endpoint (empty disables
// sending — jobs queue and wait); secret authenticates maild→edge calls.
func New(dir, edgeURL, secret string) *Queue {
	_ = os.MkdirAll(dir, 0o700)
	return &Queue{
		dir:     dir,
		edgeURL: edgeURL,
		secret:  secret,
		// Generous timeout: the edge relays the full message to the smarthost before responding, and
		// a large message (big attachments) can take well over 30s. A timeout that fires while the
		// edge has ALREADY relayed causes the job to be retried and re-delivered — duplicate mail.
		client: &http.Client{Timeout: 5 * time.Minute},
		kick:   make(chan struct{}, 1),
	}
}

// Enqueue spools a message for external delivery and nudges the flush loop.
func (q *Queue) Enqueue(from string, to []string, raw []byte) (string, error) {
	id := uniqueID()
	if err := writeFileAtomic(filepath.Join(q.dir, id+".eml"), raw); err != nil {
		return "", err
	}
	meta, _ := json.MarshalIndent(Job{ID: id, From: from, To: to, QueuedAt: time.Now().UTC()}, "", "  ")
	if err := writeFileAtomic(filepath.Join(q.dir, id+".json"), meta); err != nil {
		return "", err
	}
	select {
	case q.kick <- struct{}{}:
	default:
	}
	return id, nil
}

// Pending returns the number of spooled messages awaiting delivery.
func (q *Queue) Pending() int {
	entries, _ := os.ReadDir(q.dir)
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// EdgeConfigured reports whether an outbound edge endpoint is set.
func (q *Queue) EdgeConfigured() bool { return q.edgeURL != "" }

// Run flushes the spool periodically and whenever Enqueue nudges it. Exits on ctx.Done.
func (q *Queue) Run(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			q.flush(ctx)
		case <-q.kick:
			q.flush(ctx)
		}
	}
}

func (q *Queue) flush(ctx context.Context) {
	if q.edgeURL == "" {
		return // no edge yet — leave jobs spooled
	}
	entries, _ := os.ReadDir(q.dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if err := q.deliver(ctx, id); err != nil {
			log.Printf("maild: outbound %s deferred: %v", id, err)
		}
	}
}

func (q *Queue) deliver(ctx context.Context, id string) error {
	metaPath := filepath.Join(q.dir, id+".json")
	rawPath := filepath.Join(q.dir, id+".eml")
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var job Job
	if err := json.Unmarshal(mb, &job); err != nil {
		return err
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.edgeURL, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("X-Mail-Edge-Secret", q.secret)
	req.Header.Set("X-Mail-From", job.From)
	req.Header.Set("X-Mail-Rcpt", strings.Join(job.To, ","))
	resp, err := q.client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			_ = os.Remove(rawPath)
			_ = os.Remove(metaPath)
			return nil
		}
	}
	// Failure — a transport error (incl. timeout) OR a non-2xx status. Count the attempt and persist
	// it; after maxAttempts, dead-letter the job instead of retrying forever. Counting on the
	// transport-error path too is the crucial fix: previously a timeout returned without bumping
	// Attempts, so the job retried (and re-delivered) endlessly.
	job.Attempts++
	if job.Attempts >= maxAttempts {
		q.deadLetter(id, metaPath, rawPath, job)
		if err != nil {
			return fmt.Errorf("gave up after %d attempts: %w", job.Attempts, err)
		}
		return fmt.Errorf("gave up after %d attempts: edge returned %d", job.Attempts, resp.StatusCode)
	}
	nb, _ := json.MarshalIndent(job, "", "  ")
	_ = writeFileAtomic(metaPath, nb)
	if err != nil {
		return err
	}
	return fmt.Errorf("edge returned %d", resp.StatusCode)
}

// deadLetter moves a permanently-failed job out of the active spool (into a "failed" subdir, which
// flush ignores) so it stops being retried. The bytes are preserved for inspection rather than
// silently dropped.
func (q *Queue) deadLetter(id, metaPath, rawPath string, job Job) {
	failedDir := filepath.Join(q.dir, "failed")
	if err := os.MkdirAll(failedDir, 0o700); err == nil {
		_ = os.Rename(rawPath, filepath.Join(failedDir, id+".eml"))
		_ = os.Rename(metaPath, filepath.Join(failedDir, id+".json"))
	} else {
		_ = os.Remove(rawPath)
		_ = os.Remove(metaPath)
	}
	log.Printf("maild: outbound %s DEAD-LETTERED after %d attempts (from=%s to=%v)", id, job.Attempts, job.From, job.To)
}

func uniqueID() string {
	now := time.Now()
	return fmt.Sprintf("%d-%09d-%d", now.Unix(), now.Nanosecond(), os.Getpid())
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
