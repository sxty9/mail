import { useEffect, useRef, useState } from 'react';
import {
  Avatar,
  Box,
  Button,
  CheckIcon,
  DropdownMenu,
  EmptyState,
  EyeOffIcon,
  GlobeIcon,
  IconButton,
  KeyIcon,
  MailIcon,
  MoveIcon,
  PencilIcon,
  SearchField,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  XIcon,
  useLiveQuery,
  userHasRight,
  type MenuItem,
  type ServiceApiClient,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Info, MailboxesResp, MessageFull, MessageMeta, MessagesResp } from './types';
import { bareAddress, folderLabel, forwardDefaults, replyDefaults } from './helpers';
import { FolderSidebar } from './FolderSidebar';
import { MessageList } from './MessageList';
import { ReadingPane } from './ReadingPane';
import { Composer, type ComposeState, type ComposerHandle } from './Composer';
import { AdminPanel } from './AdminPanel';
import { AppPasswordsModal } from './AppPasswordsModal';

const READ = 'hp_mail_read';
const SEND = 'hp_mail_send';
const ADMIN = 'hp_mail_admin';

type RightView = { kind: 'read' } | { kind: 'compose'; state: ComposeState; seq: number };

export function MailApp({ user, api, ui, nav, instance }: ServiceContextProps) {
  useEffect(() => {
    nav.setTitle('Mail');
  }, [nav]);

  const canRead = userHasRight(user, READ);
  const canSend = userHasRight(user, SEND);
  const canAdmin = userHasRight(user, ADMIN);

  const [folder, setFolder] = useState<string>('INBOX');
  const [search, setSearch] = useState('');
  const [openId, setOpenId] = useState<string | null>(null);
  const [view, setView] = useState<RightView>({ kind: 'read' });
  const [appsOpen, setAppsOpen] = useState(false);
  const [adminOpen, setAdminOpen] = useState(false);
  const [composeSeq, setComposeSeq] = useState(0);
  const [composeDirty, setComposeDirty] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [anchorId, setAnchorId] = useState<string | null>(null);
  const composerRef = useRef<ComposerHandle>(null);

  const info = useLiveInfo(api, canRead);
  const boxes = useBoxes(api, canRead);
  const list = useList(api, folder, canRead);

  if (!canRead) {
    return (
      <EmptyState
        icon={<MailIcon />}
        title="No mailbox access"
        description="An admin can grant the “Use mail” right in the Rights service."
      />
    );
  }

  const addresses = info?.addresses?.length ? info.addresses : [info?.address || user.username];
  const folders = boxes?.data?.folders ?? [];
  const messages = list?.data?.messages ?? [];
  const q = search.trim().toLowerCase();
  const rows = q ? messages.filter((m) => `${m.subject} ${m.from} ${m.to}`.toLowerCase().includes(q)) : messages;
  const showRecipient = folder === 'Sent' || folder === 'Drafts';

  // leaveCompose auto-saves an unsaved compose to Drafts before the inline composer is replaced, so
  // the user never loses work and is never forced to discard or send (the Discard button is the
  // explicit way to throw a message away).
  async function leaveCompose() {
    if (view.kind === 'compose' && composeDirty) {
      await composerRef.current?.saveDraft();
      refreshAll();
    }
    setComposeDirty(false);
  }

  async function selectFolder(f: string) {
    await leaveCompose();
    setFolder(f);
    setOpenId(null);
    setView({ kind: 'read' });
    setSelected(new Set());
    setAnchorId(null);
  }

  async function openMessage(m: MessageMeta) {
    await leaveCompose();
    // A plain click opens a single message and clears any multi-selection.
    setSelected(new Set());
    setAnchorId(m.id);
    // Opening a draft resumes editing it in the composer rather than just reading it.
    if (folder === 'Drafts') {
      void openDraft(m.id);
      return;
    }
    setOpenId(m.id);
    setView({ kind: 'read' });
    if (!m.seen) {
      try {
        await api.post('flags', { mailbox: folder, id: m.id, seen: true });
        list?.refresh();
        boxes?.refresh();
      } catch {
        /* non-fatal */
      }
    }
  }

  async function openDraft(id: string) {
    try {
      const full = await api.get<MessageFull>(`message?mailbox=Drafts&id=${encodeURIComponent(id)}`);
      const fromAddr = bareAddress(full.from);
      startCompose({
        from: addresses.includes(fromAddr) ? fromAddr : addresses[0],
        to: full.to,
        cc: full.cc,
        bcc: full.bcc,
        subject: full.subject,
        html: full.html,
        text: full.text,
        inReplyTo: full.inReplyTo,
        references: full.references,
        draftId: id,
        keepAttachments: full.attachments,
      });
    } catch (e) {
      ui.toast({ title: 'Could not open draft', description: (e as Error).message, variant: 'error' });
    }
  }

  function startCompose(state: ComposeState) {
    setComposeSeq((n) => n + 1);
    setComposeDirty(false);
    setView({ kind: 'compose', state, seq: composeSeq + 1 });
  }

  async function compose(make: () => ComposeState) {
    await leaveCompose();
    startCompose(make());
  }

  function newMessage() {
    compose(() => ({ from: addresses[0], to: '', cc: '', bcc: '', subject: '', html: '', text: '' }));
  }

  function startReply(full: MessageFull) {
    const { html, text } = replyDefaults(full);
    startCompose({
      from: addresses[0],
      to: full.from,
      cc: '',
      bcc: '',
      subject: /^re:/i.test(full.subject) ? full.subject : `Re: ${full.subject}`,
      html,
      text,
      inReplyTo: full.messageId,
      references: [...(full.references ?? []), full.messageId].filter(Boolean),
    });
  }

  function startForward(full: MessageFull) {
    const { html, text } = forwardDefaults(full);
    startCompose({
      from: addresses[0],
      to: '',
      cc: '',
      bcc: '',
      subject: /^fwd:/i.test(full.subject) ? full.subject : `Fwd: ${full.subject}`,
      html,
      text,
    });
  }

  function refreshAll() {
    list?.refresh();
    boxes?.refresh();
  }

  // ── multi-select (Cmd/Ctrl-click toggles, Shift-click selects a range) ────────────────
  function clearSelection() {
    setSelected(new Set());
  }

  function toggleSelect(m: MessageMeta) {
    setSelected((cur) => {
      const next = new Set(cur);
      if (next.has(m.id)) next.delete(m.id);
      else next.add(m.id);
      return next;
    });
    setAnchorId(m.id);
  }

  function rangeSelect(m: MessageMeta) {
    const ids = rows.map((x) => x.id);
    const b = ids.indexOf(m.id);
    const a = anchorId ? ids.indexOf(anchorId) : -1;
    if (a < 0 || b < 0) {
      toggleSelect(m);
      return;
    }
    const [lo, hi] = a < b ? [a, b] : [b, a];
    const span = ids.slice(lo, hi + 1);
    setSelected((cur) => {
      const next = new Set(cur);
      span.forEach((id) => next.add(id));
      return next;
    });
  }

  async function bulkEach(fn: (id: string) => Promise<void>) {
    const ids = [...selected];
    for (const id of ids) {
      try {
        await fn(id);
      } catch {
        /* keep going; a per-item failure shouldn't abort the batch */
      }
    }
    if (openId && selected.has(openId)) {
      setOpenId(null);
      setView({ kind: 'read' });
    }
    clearSelection();
    refreshAll();
  }

  const bulkMark = (seen: boolean) => bulkEach((id) => api.post('flags', { mailbox: folder, id, seen }).then(() => undefined));
  const bulkDelete = () => bulkEach((id) => api.post('delete', { mailbox: folder, id }).then(() => undefined));
  const bulkMove = (to: string) => bulkEach((id) => api.post('move', { mailbox: folder, id, to }).then(() => undefined));

  const bulkMoveTargets: MenuItem[] = folders
    .filter((f) => f.name !== folder)
    .map((f) => ({ id: f.name, label: folderLabel(f.name), onSelect: () => bulkMove(f.name) }));

  return (
    <Box className="flex h-full min-h-0 flex-col gap-3 px-6 py-5">
      <Stack direction="row" align="center" justify="between" gap={3} wrap className="shrink-0">
        <Stack direction="row" align="center" gap={2} className="min-w-0">
          <Avatar name={user.displayName || user.username} src={user.avatarUrl} size={32} />
          <Text variant="title3" weight="semibold" truncate>
            {boxes?.data?.address || user.username}
          </Text>
        </Stack>
        <Stack direction="row" align="center" gap={2} wrap>
          <SearchField value={search} onChange={setSearch} placeholder="Search mail" />
          {canAdmin && (
            <Button variant="secondary" iconLeft={<GlobeIcon />} onClick={() => setAdminOpen(true)}>
              Admin
            </Button>
          )}
          <Button variant="secondary" iconLeft={<KeyIcon />} onClick={() => setAppsOpen(true)}>
            Mail apps
          </Button>
          {canSend && (
            <Button variant="primary" iconLeft={<PencilIcon />} onClick={newMessage}>
              Compose
            </Button>
          )}
        </Stack>
      </Stack>

      <Box className="flex min-h-0 flex-1 overflow-hidden rounded-xl border border-separator bg-surface">
        <Box className="w-56 shrink-0 overflow-auto border-r border-separator p-2">
          <FolderSidebar
            api={api}
            ui={ui}
            folders={folders}
            active={folder}
            onSelect={selectFolder}
            onChanged={() => boxes?.refresh()}
          />
        </Box>

        <Box className="flex w-[340px] shrink-0 flex-col border-r border-separator">
          {selected.size > 0 && (
            <Stack direction="row" align="center" justify="between" gap={2} className="shrink-0 border-b border-separator bg-accent/10 px-3 py-2">
              <Text variant="footnote" weight="medium">
                {selected.size} selected
              </Text>
              <Stack direction="row" align="center" gap={1}>
                <IconButton label="Mark read" size="sm" variant="ghost" onClick={() => bulkMark(true)}>
                  <CheckIcon className="h-4 w-4" />
                </IconButton>
                <IconButton label="Mark unread" size="sm" variant="ghost" onClick={() => bulkMark(false)}>
                  <EyeOffIcon className="h-4 w-4" />
                </IconButton>
                {bulkMoveTargets.length > 0 && (
                  <DropdownMenu
                    align="end"
                    items={bulkMoveTargets}
                    trigger={
                      <IconButton label="Move to…" size="sm" variant="ghost">
                        <MoveIcon className="h-4 w-4" />
                      </IconButton>
                    }
                  />
                )}
                <IconButton label="Delete" size="sm" variant="ghost" onClick={bulkDelete}>
                  <TrashIcon className="h-4 w-4" />
                </IconButton>
                <IconButton label="Clear selection" size="sm" variant="ghost" onClick={clearSelection}>
                  <XIcon className="h-4 w-4" />
                </IconButton>
              </Stack>
            </Stack>
          )}
          <Box className="min-h-0 flex-1 overflow-auto">
            {list?.loading && !list?.data ? (
              <Stack align="center" className="py-16">
                <Spinner />
              </Stack>
            ) : (
              <MessageList
                messages={rows}
                activeId={openId}
                selected={selected}
                showRecipient={showRecipient}
                query={q}
                onOpen={openMessage}
                onToggle={toggleSelect}
                onRange={rangeSelect}
              />
            )}
          </Box>
        </Box>

        <Box
          className={`flex flex-col overflow-hidden ${
            expanded
              ? 'fixed inset-x-[5%] inset-y-[5%] z-50 rounded-2xl border border-separator bg-surface-raised shadow-elev-3'
              : 'min-w-0 flex-1'
          }`}
        >
          {view.kind === 'compose' ? (
            <Composer
              key={view.seq}
              ref={composerRef}
              api={api}
              ui={ui}
              state={view.state}
              addresses={addresses}
              onDirtyChange={setComposeDirty}
              onExpand={() => setExpanded((v) => !v)}
              expanded={expanded}
              onClose={() => {
                setComposeDirty(false);
                setExpanded(false);
                setView({ kind: 'read' });
              }}
              onSent={() => {
                setComposeDirty(false);
                setExpanded(false);
                setView({ kind: 'read' });
                refreshAll();
              }}
            />
          ) : (
            <ReadingPane
              api={api}
              ui={ui}
              folder={folder}
              id={openId}
              folders={folders}
              onChanged={refreshAll}
              onReply={canSend ? startReply : undefined}
              onForward={canSend ? startForward : undefined}
              onExpand={openId ? () => setExpanded((v) => !v) : undefined}
              expanded={expanded}
            />
          )}
        </Box>
      </Box>

      {expanded && <Box className="fixed inset-0 z-40 bg-black/60" onClick={() => setExpanded(false)} />}

      {appsOpen && <AppPasswordsModal api={api} ui={ui} instance={instance} onClose={() => setAppsOpen(false)} />}
      {adminOpen && <AdminPanel api={api} ui={ui} onClose={() => setAdminOpen(false)} />}
    </Box>
  );
}

// ── small query hooks (thin wrappers over useLiveQuery, gated by the read right) ───────

function useLiveInfo(api: ServiceApiClient, enabled: boolean): Info | null {
  const q = useLiveQuery<Info>(() => api.get<Info>('info'), 60000, [enabled]);
  return enabled ? q.data : null;
}

function useBoxes(api: ServiceApiClient, enabled: boolean) {
  const q = useLiveQuery<MailboxesResp>(() => api.get<MailboxesResp>('mailboxes'), 15000, [enabled]);
  return enabled ? q : null;
}

function useList(api: ServiceApiClient, folder: string, enabled: boolean) {
  const q = useLiveQuery<MessagesResp>(() => api.get<MessagesResp>(`messages?mailbox=${encodeURIComponent(folder)}`), 8000, [folder, enabled]);
  return enabled ? q : null;
}
