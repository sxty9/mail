# holistic-mail (`maild`)

The **mail service** for the Holistic ecosystem. Every Holistic user (a Linux account — the
single source of truth) automatically has the address `<username>@<mailDomain>`, where
`mailDomain` is the canonical domain owned by the dashboard (`GET /api/instance`). A Go daemon
stores mail in per-user **Maildirs** and serves an HTTP/JMAP-style API; a `@holistic/ui` plugin
is the web client.

```
                Internet (Gmail/Outlook/…)
                       │ SMTP ↕ MX
              Cloudflare Email Routing/Worker  ──►  POST /api/services/mail/inbound
                       ▲                                       │ (sxgate mail edge)
 Browser ─https─► Caddy (holistic.local) ──────────┐          ▼
   ├─ /                       → holistic SPA (this plugin)   maild (127.0.0.1:8775)
   ├─ /api/*                  → holistic backend  (8770)     • Maildir store (SoT)
   └─ /api/services/mail/*    → maild             (8775)     • local delivery (no SMTP)
                                                             • outbound spool ─► sxgate edge ─► smarthost
```

- **Single sign-on:** validates the same holistic session (HS256 JWT in the `h_access` cookie,
  secret `/etc/holistic/jwt-secret`) — no separate login.
- **Identity = Linux (single source of truth):** usernames are Linux accounts; admin = `sudo`.
  The mail domain is read (never invented) from `/var/lib/holistic/instance.json`.
- **Least privilege:** runs as the unprivileged `mail` system user; performs no escalation.

## Protocol & transport

Holistic's public ingress (sxgate → Cloudflare Tunnel) is **HTTP-only**, so mail is
HTTP/JMAP-native:

- **Internal mail** (Holistic ↔ Holistic) is delivered **directly into the recipient's Maildir** —
  no SMTP involved.
- **External mail** is owned by the **sxgate mail edge** (a separate workstream): inbound arrives
  as an authenticated HTTPS webhook (`/inbound`); outbound is spooled and handed to the edge,
  which DKIM-signs and relays via a smarthost. With no edge configured, internal mail works fully
  and external recipients simply queue.
- Storage is **Maildir**, so a LAN **IMAP** server (Dovecot) can be added later over the same
  mailboxes without migration.

## Prerequisites

The [holistic](https://github.com/sxty9/holistic) repo must be present **as a sibling**
(`../holistic`) with the dashboard installed — it provides the `@holistic/ui` SDK + the SPA, the
shared JWT secret and `/var/lib/holistic/instance.json`.

## Quickstart

```bash
cd mail
sudo ./service setup     # build maild, wire systemd + Caddy, declare rights, link + rebuild the SPA
```

After `setup`, **Mail** appears in the holistic sidebar. Other commands: `service build`,
`service start|stop|restart`, `service status`, `service update`, `service uninstall [--purge]`.

## Rights (privleg)

Declared in `permissions/mail.json`; each is backed 1:1 by an `hp_*` Linux group and enforced as
`isAdmin || group ∈ user.groups` in both backend and UI.

| Right | Group | Default | Gates |
|---|---|---|---|
| Use mail | `hp_mail_read` | **true** | read + manage your own mailbox |
| Send mail | `hp_mail_send` | **true** | compose/send from your address |
| Administer mail | `hp_mail_admin` | false (`dangerous`) | other mailboxes, domains, quotas |
| Read mail audit | `hp_mail_audit` | false (`sensitive`) | delivery logs / audit trails |

`default:true` = provisioning grants everyone (privleg can revoke); `default:false` = admin-only
until privleg grants.

## API (`/api/services/mail/`)

| Method | Path | Access | Purpose |
|---|---|---|---|
| GET | `health` | none | liveness |
| GET | `info` | signed-in | identity + your address |
| GET | `mailboxes` | `hp_mail_read` | folders + unread counts |
| GET | `messages?mailbox=…` | `hp_mail_read` | message list |
| GET | `message?mailbox=…&id=…` | `hp_mail_read` | one message (headers + body) |
| POST | `send` | `hp_mail_send` + CSRF | compose & deliver/queue |
| POST | `flags` / `move` / `delete` | `hp_mail_read` + CSRF | mailbox management |
| POST | `inbound` | **edge secret** | the sxgate edge hands in internet mail |

## Storage

Maildir++ under the daemon's data root (default `/var/lib/mail/mailboxes/<user>/Maildir`, owned by
`mail`): `INBOX` plus `.Sent` / `.Drafts` / `.Trash`. Maildir is the single source of truth — no
database. Outbound spool lives at `/var/lib/mail/outbound`.

## Edge integration (env)

| Var | Meaning |
|---|---|
| `MAILD_DATA` | data root (default `/var/lib/mail`) |
| `MAILD_INBOUND_SECRET[_FILE]` | shared secret the edge presents on `/inbound` |
| `MAILD_EDGE_URL` | sxgate edge submission endpoint for outbound (unset = queue only) |
| `MAILD_EDGE_SECRET[_FILE]` | secret for maild → edge calls |
| `HOLISTIC_MAIL_DOMAIN` | overrides the canonical mail domain |

## Local development

```bash
(cd backend && go build ./... && go vet ./... && go test ./...)
ln -sfn "$PWD/ui" ../holistic/frontend/external/mail
( cd ../holistic/frontend && pnpm --filter @holistic/app dev )   # http://localhost:5173
```

UI imports are restricted to `@holistic/ui` + `react` (enforced by holistic's `eslint.services.cjs`).

## Layout

```
service                       single-file CLI: setup / build / lifecycle
permissions/mail.json         rights manifest (drop-in for privleg)
backend/                      Go daemon (maild)
  cmd/maild/                    entry point — 127.0.0.1:8775
  internal/auth/                shared-JWT validation + live group/admin resolution + CSRF (reused)
  internal/rights/              the hp_mail_* group constants
  internal/api/                 HTTP routes incl. the inbound webhook
  internal/maildir/             Maildir++ store (deliver/list/read/flags/move/delete)
  internal/message/             RFC 5322 build + parse (MIME)
  internal/instance/            reads the canonical mail domain (read-only)
  internal/lda/                 local delivery + external-vs-local routing
  internal/outbound/            disk-backed spool → sxgate edge
ui/                           @holistic/ui plugin (MailApp): inbox · reading sheet · compose
```

## License

MIT — see [LICENSE](LICENSE).
