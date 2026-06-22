import { useEffect, useState } from 'react';
import {
  Avatar,
  Badge,
  Button,
  Column,
  DataTable,
  Divider,
  EmptyState,
  Field,
  Input,
  KeyIcon,
  MailIcon,
  Modal,
  Panel,
  PencilIcon,
  ReplyIcon,
  SearchField,
  SegmentedControl,
  SendIcon,
  Sheet,
  Spinner,
  Stack,
  Text,
  Textarea,
  TrashIcon,
  useLiveQuery,
  userHasRight,
  type InstanceInfo,
  type ServiceApiClient,
  type ServiceContextProps,
  type ServiceUiBridge,
} from '@holistic/ui';
import type {
  AppPasswordCreated,
  AppPasswordsResp,
  Folder,
  MailboxesResp,
  MessageFull,
  MessageMeta,
  MessagesResp,
  SendResult,
} from './types';

const FOLDERS: Folder[] = ['INBOX', 'Sent', 'Drafts', 'Trash'];
const READ = 'hp_mail_read';
const SEND = 'hp_mail_send';

interface ComposeState {
  to: string;
  cc: string;
  subject: string;
  body: string;
  inReplyTo?: string;
  references?: string[];
}

export function MailApp({ user, api, ui, nav, instance }: ServiceContextProps) {
  useEffect(() => {
    nav.setTitle('Mail');
  }, [nav]);

  const canRead = userHasRight(user, READ);
  const canSend = userHasRight(user, SEND);

  const [folder, setFolder] = useState<Folder>('INBOX');
  const [search, setSearch] = useState('');
  const [openId, setOpenId] = useState<string | null>(null);
  const [compose, setCompose] = useState<ComposeState | null>(null);
  const [appsOpen, setAppsOpen] = useState(false);

  const boxes = useLiveQuery<MailboxesResp>(() => api.get<MailboxesResp>('mailboxes'), 15000);
  const list = useLiveQuery<MessagesResp>(() => api.get<MessagesResp>(`messages?mailbox=${folder}`), 8000, [folder]);

  if (!canRead) {
    return (
      <EmptyState
        icon={<MailIcon />}
        title="No mailbox access"
        description="An admin can grant the “Use mail” right in the Rights service."
      />
    );
  }

  const messages = list.data?.messages ?? [];
  const q = search.trim().toLowerCase();
  const rows = q
    ? messages.filter((m) => `${m.subject} ${m.from} ${m.to}`.toLowerCase().includes(q))
    : messages;
  const showRecipient = folder === 'Sent' || folder === 'Drafts';

  const columns: Column<MessageMeta>[] = [
    {
      key: 'who',
      header: showRecipient ? 'To' : 'From',
      width: '32%',
      render: (m) => {
        const who = showRecipient ? m.to : m.from;
        return (
          <Stack direction="row" align="center" gap={2}>
            <Avatar name={displayName(who)} size={26} />
            <Text truncate weight={m.seen ? 'normal' : 'semibold'}>
              {displayName(who)}
            </Text>
          </Stack>
        );
      },
    },
    {
      key: 'subject',
      header: 'Subject',
      render: (m) => (
        <Stack direction="row" align="center" gap={2}>
          {m.flagged && <Badge variant="warning">flagged</Badge>}
          <Text truncate weight={m.seen ? 'normal' : 'semibold'} color={m.seen ? 'secondary' : 'primary'}>
            {m.subject || '(no subject)'}
          </Text>
        </Stack>
      ),
    },
    {
      key: 'date',
      header: 'Date',
      align: 'right',
      width: 150,
      sortable: true,
      sortValue: (m) => m.date,
      render: (m) => (
        <Text variant="footnote" color="tertiary">
          {formatWhen(m.date)}
        </Text>
      ),
    },
  ];

  async function openMessage(m: MessageMeta) {
    setOpenId(m.id);
    if (!m.seen) {
      try {
        await api.post('flags', { mailbox: folder, id: m.id, seen: true });
        list.refresh();
        boxes.refresh();
      } catch {
        /* non-fatal: opening still works */
      }
    }
  }

  function startReply(full: MessageFull) {
    setOpenId(null);
    setCompose({
      to: full.from,
      cc: '',
      subject: /^re:/i.test(full.subject) ? full.subject : `Re: ${full.subject}`,
      body: `\n\n— On ${formatWhen(full.date)}, ${displayName(full.from)} wrote —\n${quote(full.text)}`,
      inReplyTo: full.messageId,
      references: [...(full.references ?? []), full.messageId].filter(Boolean),
    });
  }

  return (
    <Stack gap={4}>
      <Stack direction="row" align="center" justify="between" gap={3} wrap>
        <Stack direction="row" align="center" gap={2}>
          <MailIcon />
          <Text variant="title3" weight="semibold">
            {boxes.data?.address || user.username}
          </Text>
        </Stack>
        <Stack direction="row" align="center" gap={2}>
          <SearchField value={search} onChange={setSearch} placeholder="Search mail" />
          <Button variant="secondary" iconLeft={<KeyIcon />} onClick={() => setAppsOpen(true)}>
            Mail apps
          </Button>
          {canSend && (
            <Button variant="primary" iconLeft={<PencilIcon />} onClick={() => setCompose(blankCompose())}>
              Compose
            </Button>
          )}
        </Stack>
      </Stack>

      <SegmentedControl<Folder>
        value={folder}
        onChange={(f) => {
          setFolder(f);
          setOpenId(null);
        }}
        options={FOLDERS.map((f) => ({ value: f, label: folderLabel(f, boxes.data) }))}
      />

      <Panel elevation={1}>
        {list.loading && !list.data ? (
          <Stack align="center" className="py-16">
            <Spinner />
          </Stack>
        ) : (
          <DataTable<MessageMeta>
            columns={columns}
            rows={rows}
            rowKey={(m) => m.id}
            onRowClick={openMessage}
            initialSort={{ key: 'date', dir: 'desc' }}
            maxHeight={520}
            emptyState={
              <EmptyState
                icon={<MailIcon />}
                title="No messages"
                description={q ? 'Nothing matches your search.' : 'This mailbox is empty.'}
              />
            }
          />
        )}
      </Panel>

      <ReadingSheet
        api={api}
        ui={ui}
        folder={folder}
        id={openId}
        onClose={() => setOpenId(null)}
        onChanged={() => {
          list.refresh();
          boxes.refresh();
        }}
        onReply={canSend ? startReply : undefined}
      />

      {compose && (
        <ComposeModal
          api={api}
          ui={ui}
          state={compose}
          onClose={() => setCompose(null)}
          onSent={() => {
            setCompose(null);
            list.refresh();
            boxes.refresh();
          }}
        />
      )}

      {appsOpen && <AppPasswordsModal api={api} ui={ui} instance={instance} onClose={() => setAppsOpen(false)} />}
    </Stack>
  );
}

function ReadingSheet({
  api,
  ui,
  folder,
  id,
  onClose,
  onChanged,
  onReply,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  folder: Folder;
  id: string | null;
  onClose: () => void;
  onChanged: () => void;
  onReply?: (m: MessageFull) => void;
}) {
  const [msg, setMsg] = useState<MessageFull | null>(null);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!id) {
      setMsg(null);
      return;
    }
    let active = true;
    setLoading(true);
    setMsg(null);
    api
      .get<MessageFull>(`message?mailbox=${folder}&id=${encodeURIComponent(id)}`)
      .then((m) => {
        if (active) setMsg(m);
      })
      .catch(() => {
        if (active) ui.toast({ title: 'Could not load message', variant: 'error' });
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [api, ui, folder, id]);

  async function trash() {
    if (!id) return;
    try {
      await api.post('delete', { mailbox: folder, id });
      ui.toast({ title: 'Moved to Trash' });
      onChanged();
      onClose();
    } catch (e) {
      ui.toast({ title: 'Delete failed', description: (e as Error).message, variant: 'error' });
    }
  }

  return (
    <Sheet open={!!id} onOpenChange={(o) => !o && onClose()} title={msg?.subject || 'Message'}>
      {loading || !msg ? (
        <Stack align="center" className="py-16">
          <Spinner />
        </Stack>
      ) : (
        <Stack gap={3} className="p-5">
          <Stack direction="row" align="center" gap={3}>
            <Avatar name={displayName(msg.from)} size={40} />
            <Stack gap={0}>
              <Text weight="semibold">{displayName(msg.from)}</Text>
              <Text variant="footnote" color="secondary">
                {msg.from}
              </Text>
            </Stack>
          </Stack>
          <Stack gap={1}>
            <Text variant="footnote" color="tertiary">
              To: {msg.to || '—'}
              {msg.cc ? ` · Cc: ${msg.cc}` : ''}
            </Text>
            <Text variant="footnote" color="tertiary">
              {formatWhen(msg.date)}
            </Text>
          </Stack>
          <Divider />
          <Text className="whitespace-pre-wrap">{msg.text || '(empty message)'}</Text>
          <Stack direction="row" gap={2} className="pt-2">
            {onReply && (
              <Button variant="primary" iconLeft={<ReplyIcon />} onClick={() => onReply(msg)}>
                Reply
              </Button>
            )}
            <Button variant="secondary" iconLeft={<TrashIcon />} onClick={trash}>
              Delete
            </Button>
          </Stack>
        </Stack>
      )}
    </Sheet>
  );
}

function ComposeModal({
  api,
  ui,
  state,
  onClose,
  onSent,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  state: ComposeState;
  onClose: () => void;
  onSent: () => void;
}) {
  const [to, setTo] = useState(state.to);
  const [cc, setCc] = useState(state.cc);
  const [subject, setSubject] = useState(state.subject);
  const [body, setBody] = useState(state.body);
  const [busy, setBusy] = useState(false);

  async function send() {
    if (!to.trim()) {
      ui.toast({ title: 'Add at least one recipient', variant: 'error' });
      return;
    }
    setBusy(true);
    try {
      const res = await api.post<SendResult>('send', {
        to: splitAddrs(to),
        cc: splitAddrs(cc),
        subject,
        body,
        inReplyTo: state.inReplyTo,
        references: state.references,
      });
      const queued =
        res.queuedExternal > 0 && !res.edgeConfigured
          ? ' — external recipients queued (mail edge not configured yet)'
          : '';
      ui.toast({ title: `Message sent${queued}`, variant: 'success' });
      onSent();
    } catch (e) {
      ui.toast({ title: 'Send failed', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="New message"
      size="lg"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" iconLeft={<SendIcon />} loading={busy} onClick={send}>
            Send
          </Button>
        </>
      }
    >
      <Stack gap={3}>
        <Field label="To">
          <Input value={to} onChange={(e) => setTo(e.target.value)} placeholder="user@domain, …" />
        </Field>
        <Field label="Cc">
          <Input value={cc} onChange={(e) => setCc(e.target.value)} placeholder="optional" />
        </Field>
        <Field label="Subject">
          <Input value={subject} onChange={(e) => setSubject(e.target.value)} />
        </Field>
        <Field label="Message">
          <Textarea value={body} onChange={(e) => setBody(e.target.value)} rows={10} />
        </Field>
      </Stack>
    </Modal>
  );
}

function AppPasswordsModal({
  api,
  ui,
  instance,
  onClose,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  instance: InstanceInfo;
  onClose: () => void;
}) {
  const apps = useLiveQuery<AppPasswordsResp>(() => api.get<AppPasswordsResp>('apppasswords'), 30000);
  const [label, setLabel] = useState('');
  const [created, setCreated] = useState<AppPasswordCreated | null>(null);
  const [busy, setBusy] = useState(false);

  const jmapUrl = `${instance.origin || ''}/api/services/mail/jmap/session`;

  async function create() {
    setBusy(true);
    try {
      const res = await api.post<AppPasswordCreated>('apppasswords', { label: label.trim() || 'Mail app' });
      setCreated(res);
      setLabel('');
      apps.refresh();
    } catch (e) {
      ui.toast({ title: 'Could not create app password', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: string) {
    if (!(await ui.confirm({ title: 'Revoke this app password?', danger: true, confirmLabel: 'Revoke' }))) return;
    try {
      await api.post('apppasswords/delete', { id });
      apps.refresh();
    } catch (e) {
      ui.toast({ title: 'Revoke failed', description: (e as Error).message, variant: 'error' });
    }
  }

  const list = apps.data?.appPasswords ?? [];

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Mail apps (JMAP)"
      size="lg"
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <Stack gap={4}>
        <Stack gap={1}>
          <Text variant="footnote" color="tertiary">JMAP URL</Text>
          <Text className="break-all">{jmapUrl}</Text>
          <Text variant="footnote" color="tertiary">Username</Text>
          <Text>{apps.data?.username || ''}</Text>
        </Stack>
        <Divider />
        <Stack direction="row" gap={2} align="end">
          <Field label="New app password" className="flex-1">
            <Input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="e.g. Laptop aerc" />
          </Field>
          <Button variant="primary" loading={busy} onClick={create}>
            Create
          </Button>
        </Stack>
        {created && (
          <Panel elevation={1} className="p-3">
            <Stack gap={1}>
              <Text weight="semibold">Copy this password now — it is shown only once:</Text>
              <Text className="break-all" color="accent">
                {created.token}
              </Text>
            </Stack>
          </Panel>
        )}
        <Stack gap={2}>
          {list.map((p) => (
            <Stack key={p.id} direction="row" align="center" justify="between" gap={2}>
              <Text>{p.label}</Text>
              <Button size="sm" variant="ghost" iconLeft={<TrashIcon />} onClick={() => revoke(p.id)}>
                Revoke
              </Button>
            </Stack>
          ))}
          {list.length === 0 && (
            <Text variant="footnote" color="tertiary">
              No app passwords yet.
            </Text>
          )}
        </Stack>
      </Stack>
    </Modal>
  );
}

// ── helpers ──────────────────────────────────────────────────────────────────────────

function blankCompose(): ComposeState {
  return { to: '', cc: '', subject: '', body: '' };
}

function displayName(addr: string): string {
  if (!addr) return '(unknown)';
  const m = addr.match(/^\s*"?([^"<]*)"?\s*<([^>]+)>/);
  if (m && m[1].trim()) return m[1].trim();
  const inner = m ? m[2] : addr;
  const at = inner.indexOf('@');
  return at > 0 ? inner.slice(0, at) : inner.trim();
}

function folderLabel(f: Folder, boxes: MailboxesResp | null | undefined): string {
  const name = f === 'INBOX' ? 'Inbox' : f;
  const info = boxes?.folders.find((x) => x.name === f);
  return info && info.unread > 0 ? `${name} · ${info.unread}` : name;
}

function formatWhen(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' });
}

function splitAddrs(s: string): string[] {
  return s
    .split(/[,;]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

function quote(text: string): string {
  return text
    .split('\n')
    .map((l) => `> ${l}`)
    .join('\n');
}
