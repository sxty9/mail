// Command maild is the holistic mail service daemon. It exposes an HTTP surface under
// /api/services/mail/, validates the shared holistic session (a signed JWT in the h_access
// cookie) without any RPC to the holistic backend, stores mail in per-user Maildirs, and
// delivers internal mail directly (no SMTP). Public send/receive is owned by the sxgate mail
// edge, which maild reaches over the outbound queue and the inbound webhook. It runs
// unprivileged behind the holistic Caddy proxy.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mail/internal/api"
	"mail/internal/apppass"
	"mail/internal/auth"
	"mail/internal/instance"
	"mail/internal/lda"
	"mail/internal/maildir"
	"mail/internal/outbound"
	"mail/internal/profile"
	"mail/internal/registry"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8775", "address to listen on")
	flag.Parse()

	secret, err := auth.LoadSecret()
	if err != nil {
		log.Fatalf("maild: %v", err)
	}
	// Admin = membership in this group (the single Linux source of truth). The systemd unit
	// sets MAIL_ADMIN_GROUP; the verifier defaults to "sudo" when it is empty.
	v := auth.NewVerifier(secret, os.Getenv("MAIL_ADMIN_GROUP"))

	dataRoot := getenv("MAILD_DATA", "/var/lib/mail")
	store := maildir.New(filepath.Join(dataRoot, "mailboxes"))
	inst := instance.New()

	// Outbound + inbound are the contract with the sxgate mail edge. Both are optional: with
	// no edge configured, internal mail still works and external mail simply queues.
	out := outbound.New(
		filepath.Join(dataRoot, "outbound"),
		os.Getenv("MAILD_EDGE_URL"),
		readSecret("MAILD_EDGE_SECRET", "MAILD_EDGE_SECRET_FILE"),
	)
	inboundSecret := readSecret("MAILD_INBOUND_SECRET", "MAILD_INBOUND_SECRET_FILE")
	// Optional icaly calendar integration. All empty by default → maild behaves unchanged.
	// The internal-send and inbound-forward secrets are the same shared icaly↔maild secret.
	internalSecret := readSecret("MAILD_INTERNAL_SECRET", "MAILD_INTERNAL_SECRET_FILE")
	calURL := getenv("MAILD_CALENDAR_URL", "")
	calSecret := readSecret("MAILD_CALENDAR_SECRET", "MAILD_CALENDAR_SECRET_FILE")
	ap := apppass.New(filepath.Join(dataRoot, "apppasswords"))
	prof := profile.New()
	reg := registry.New(filepath.Join(dataRoot, "registry"), inst)
	del := lda.New(store, out, prof, reg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go out.Run(ctx)
	go runTrashSweep(ctx, store, trashRetention())

	srv := &http.Server{
		Handler:           api.New(v, store, inst, reg, del, ap, inboundSecret, internalSecret, calURL, calSecret).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bind synchronously so an "address in use" surfaces here, not in a goroutine.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("maild: listen %s: %v", *listen, err)
	}
	go func() {
		log.Printf("maild listening on %s (data=%s, mailDomain=%q, edge=%v)", *listen, dataRoot, inst.MailDomain(), out.EdgeConfigured())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("maild: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	log.Print("maild stopped")
}

// trashRetention is how long a deleted message lingers in Trash before the sweeper removes it for
// good. Defaults to 30 days; MAILD_TRASH_RETENTION_DAYS overrides it (0 disables auto-expiry).
func trashRetention() time.Duration {
	days := 30
	if v := strings.TrimSpace(os.Getenv("MAILD_TRASH_RETENTION_DAYS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

// runTrashSweep periodically purges messages that have sat in Trash past the retention window.
// It sweeps once at startup, then every 6 hours — Trash cleanup is housekeeping, not latency
// sensitive. Exits on ctx.Done. A non-positive retention disables it entirely.
func runTrashSweep(ctx context.Context, store *maildir.Store, retention time.Duration) {
	if retention <= 0 {
		log.Print("maild: Trash auto-expiry disabled (MAILD_TRASH_RETENTION_DAYS=0)")
		return
	}
	log.Printf("maild: Trash auto-expiry after %s", retention)
	sweep := func() {
		if n, err := store.PurgeExpiredTrash(retention); err != nil {
			log.Printf("maild: trash sweep error: %v", err)
		} else if n > 0 {
			log.Printf("maild: trash sweep purged %d expired message(s)", n)
		}
	}
	sweep()
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// readSecret returns a secret from the env var, else from the file named by fileEnv.
func readSecret(env, fileEnv string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	if path := strings.TrimSpace(os.Getenv(fileEnv)); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}
