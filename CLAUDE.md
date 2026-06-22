# CLAUDE.md

`maild` — the **holistic mail service**. Go daemon + `@holistic/ui` plugin. The SDK is **consumed
only** (never vendored). Built from `holistic-service-template`; see the root `CLAUDE.MD` for the
ecosystem maxims this must obey.

## Architecture in one line

Per-user **Maildir** is the single source of truth; **internal** mail is delivered directly (no
SMTP); **external** send/receive is owned by the **sxgate mail edge** — `maild` never speaks SMTP
to the public internet, it only exposes an inbound webhook and an outbound spool.

## Where things are

- `service` — the CLI (auto-detects id from `permissions/mail.json`; generates the systemd unit,
  Caddy route, rights drop-in). Daemon listens on `127.0.0.1:8775`; data root `/var/lib/mail`.
- `backend/internal/auth/auth.go` — shared-JWT (`h_access`) validation + live OS group/admin
  resolution + CSRF. Service-agnostic; reuse as-is.
- `backend/internal/api/api.go` — HTTP surface under `/api/services/mail/`. `guard(perm, csrf, h)`
  does auth → optional right → optional CSRF. `inbound` is the exception: authenticated by the
  shared **edge secret**, not the user JWT.
- `backend/internal/rights/rights.go` — the `hp_mail_*` group constants; mirror `permissions/mail.json`.
- `backend/internal/maildir/` — Maildir++ store (deliver/list/read/flags/move/delete). No DB.
- `backend/internal/message/` — RFC 5322 build + MIME parse (stdlib only).
- `backend/internal/instance/` — reads the canonical mail domain from `/var/lib/holistic/instance.json` (read-only).
- `backend/internal/lda/` — local delivery + local-vs-external routing.
- `backend/internal/outbound/` — disk-backed spool that hands messages to the sxgate edge.
- `ui/index.tsx` — default-exports the `ServicePlugin` (`id: 'mail'`).
- `ui/MailApp.tsx` — the plugin UI (inbox list · reading Sheet · compose Modal); renders only `@holistic/ui`.

## Rules

- Enforce every right as `isAdmin || group ∈ user.groups`, in backend AND UI. Keep three in sync:
  `permissions/mail.json` ⇄ `internal/rights` ⇄ the UI right constants (`hp_mail_read`/`hp_mail_send`).
- Never invent identity or the mail domain — Linux accounts + `instance.json` are authoritative.
- External transport belongs to sxgate. Do not add an SMTP/IMAP listener to `maild` without
  revisiting the architecture (Maildir keeps a future LAN Dovecot-IMAP layer open).
- UI may import only `@holistic/ui` and `react` (holistic's `eslint.services.cjs` enforces it).
- Daemon runs unprivileged. The systemd unit must keep `ReadWritePaths=/var/lib/mail` (writes mail).

## Edge contract (sxgate ⇄ maild)

- Inbound: `POST /api/services/mail/inbound`, header `X-Mail-Inbound-Secret`, body raw RFC 5322,
  recipients in `X-Mail-Rcpt`. Configured via `MAILD_INBOUND_SECRET[_FILE]`.
- Outbound: spool → `MAILD_EDGE_URL` with `X-Mail-Edge-Secret` (`MAILD_EDGE_SECRET[_FILE]`). Unset
  = queue only (internal mail still works).

## Verify (from the repo root)

```bash
(cd backend && go build ./... && go vet ./... && go test ./...)
python3 ../holistic/services/dashboard/lib/holistic-perms.py validate ./permissions
# UI: lint (import allowlist) + typecheck against the SDK
( cd ../holistic/frontend/app && CI=true ../node_modules/.bin/eslint --no-eslintrc -c ../eslint.services.cjs \
    --resolve-plugins-relative-to .. --ext .ts,.tsx ../external/mail )
```
