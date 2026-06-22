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
	ap := apppass.New(filepath.Join(dataRoot, "apppasswords"))
	del := lda.New(store, out, inst)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go out.Run(ctx)

	srv := &http.Server{
		Handler:           api.New(v, store, inst, del, ap, inboundSecret).Handler(),
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
