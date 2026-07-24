import { useCallback, useEffect, useRef, useState } from 'react';
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
  type ContactOption,
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

// Persisted display-mode preference: whether the reader/composer opens in the maximized "larger
// window" (Vollbild) or the inline side view. Once the user maximizes, every subsequent compose/read
// reopens maximized until they shrink it again — across sending, saving a draft, closing, switching
// folders, and page reloads.
//
// Two layers, because neither alone covers every case:
//   • a MODULE-LEVEL cache — survives a full remount of MailApp within the same page session (the
//     dashboard can remount a service plugin on navigation, which would otherwise reset useState),
//     and works even if the browser blocks storage.
//   • localStorage — carries the preference across page reloads / new sessions.
// Both are best-effort; storage errors are swallowed and the module cache still holds the value for
// the current page session.
const EXPAND_PREF_KEY = 'maild:expandView';

let expandPrefCache: boolean | null = null;

function readExpandPref(): boolean {
  if (expandPrefCache !== null) return expandPrefCache;
  try {
    expandPrefCache = localStorage.getItem(EXPAND_PREF_KEY) === '1';
  } catch {
    expandPrefCache = false;
  }
  return expandPrefCache;
}

function writeExpandPref(v: boolean): void {
  expandPrefCache = v;
  try {
    localStorage.setItem(EXPAND_PREF_KEY, v ? '1' : '0');
  } catch {
    /* best-effort; the module cache still holds it for this page session */
  }
}

function usePersistentExpand(): [boolean, (v: boolean | ((prev: boolean) => boolean)) => void] {
  const [expand, setExpand] = useState<boolean>(readExpandPref);
  const set = useCallback((v: boolean | ((prev: boolean) => boolean)) => {
    setExpand((prev) => {
      const next = typeof v === 'function' ? v(prev) : v;
      writeExpandPref(next);
      return next;
    });
  }, []);
  return [expand, set];
}

type RightView = { kind: 'read' } | { kind: 'compose'; state: ComposeState; seq: number };

export function MailApp({ user, api, apiFor, ui, nav, instance }: ServiceContextProps) {
  useEffect(() => {
    nav.setTitle('Mail');
  }, [nav]);

  const canRead = userHasRight(user, READ);
  const canSend = userHasRight(user, SEND);
  const canAdmin = userHasRight(user, ADMIN);

  // contax is the directory access point for people/recipients. We reuse it here so To/Cc/Bcc get the
  // same avatar type-ahead (and personal-group expansion) as icaly's attendee picker, rather than a
  // second address path. Groups arrive via includeGroups=1 and are expanded to member addresses on pick.
  const searchContacts = useCallback(
    async (q: string): Promise<ContactOption[]> => {
      try {
        const res = await apiFor('contax').get<{
          contacts?: ContactOption[];
          groups?: { id: string; name: string; memberCount: number }[];
        }>(`lookup?q=${encodeURIComponent(q)}&includeGroups=1`);
        const groups: ContactOption[] = (res.groups ?? []).map((g) => ({
          email: '',
          displayName: g.name,
          kind: 'group',
          groupId: g.id,
          memberCount: g.memberCount,
        }));
        return [...groups, ...(res.contacts ?? [])];
      } catch {
        return [];
      }
    },
    [apiFor],
  );
  const expandGroup = useCallback(
    async (groupId: string): Promise<ContactOption[]> => {
      try {
        const res = await apiFor('contax').get<{ contacts?: ContactOption[] }>(`groups/${encodeURIComponent(groupId)}/members`);
        return res.contacts ?? [];
      } catch {
        return [];
      }
    },
    [apiFor],
  );

  const [folder, setFolder] = useState<string>('INBOX');
  const [search, setSearch] = useState('');
  const [openId, setOpenId] = useState<string | null>(null);
  const [view, setView] = useState<RightView>({ kind: 'read' });
  const [appsOpen, setAppsOpen] = useState(false);
  const [adminOpen, setAdminOpen] = useState(false);
  const [composeSeq, setComposeSeq] = useState(0);
  const [composeDirty, setComposeDirty] = useState(false);
  const [expandPref, setExpandPref] = usePersistentExpand();
  // Reading is the exception to the saved display mode: opening a message from the list must NEVER
  // jump straight to full screen — that stays a manual, per-message choice. readerExpanded tracks
  // only the currently-open message and is reset whenever the open message changes, so every new
  // selection starts in the side view even when the saved preference is "maximized".
  const [readerExpanded, setReaderExpanded] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [anchorId, setAnchorId] = useState<string | null>(null);
  const composerRef = useRef<ComposerHandle>(null);

  const info = useLiveInfo(api, canRead);
  const boxes = useBoxes(api, canRead);
  const list = useList(api, folder, canRead);

  // Selecting another message drops back to the side view (the reading full-screen never auto-applies).
  useEffect(() => {
    setReaderExpanded(false);
  }, [openId]);

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

  // Compose auto-restores the saved display mode (Vollbild vs. side view); reading never does —
  // it only follows the manual, per-message readerExpanded. Gating on actual content also keeps a
  // close/send from leaving a floating empty full-screen box behind.
  const expanded = view.kind === 'compose' ? expandPref : openId != null && readerExpanded;

  // collapse shrinks whichever pane is maximized and records the side view as the saved preference
  // (used by the dimmed backdrop click). Reading still writes the shared state, per "save it anyway".
  const collapse = () => {
    setReaderExpanded(false);
    setExpandPref(false);
  };

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
    setAnchorId(m.id);
    // Opening a draft resumes editing it in the composer rather than just reading it.
    if (folder === 'Drafts') {
      setSelected(new Set());
      void openDraft(m.id);
      return;
    }
    // A plain click opens a message AND makes it the selection — so the highlighted row and the
    // "N selected" count always agree (no more "0 selected" while a row is highlighted).
    setSelected(new Set([m.id]));
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

  // Drag-and-drop: messages dragged from the list onto a folder are moved there.
  async function moveMessagesTo(target: string, ids: string[]) {
    if (target === folder || !ids.length) return;
    for (const id of ids) {
      try {
        await api.post('move', { mailbox: folder, id, to: target });
      } catch {
        /* keep going */
      }
    }
    if (openId && ids.includes(openId)) {
      setOpenId(null);
      setView({ kind: 'read' });
    }
    clearSelection();
    refreshAll();
    ui.toast({ title: `Moved to ${folderLabel(target)}` });
  }

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
            onDropMessages={moveMessagesTo}
          />
        </Box>

        <Box className="flex w-[340px] shrink-0 flex-col border-r border-separator">
          <Stack
            direction="row"
            align="center"
            justify="between"
            gap={2}
            className={`shrink-0 border-b border-separator px-3 py-2 ${selected.size > 0 ? 'bg-accent/10' : ''}`}
          >
            <Text variant="footnote" weight="medium" color={selected.size > 0 ? 'primary' : 'tertiary'}>
              {selected.size} selected
            </Text>
            {selected.size > 0 && (
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
            )}
          </Stack>
          <Box
            className="min-h-0 flex-1 overflow-auto"
            onClick={(e) => {
              // Clicking the empty area (not a row) deselects everything.
              if (e.target === e.currentTarget) {
                setSelected(new Set());
                setOpenId(null);
              }
            }}
          >
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
              searchContacts={searchContacts}
              expandGroup={expandGroup}
              onDirtyChange={setComposeDirty}
              onExpand={() => setExpandPref((v) => !v)}
              expanded={expanded}
              onClose={() => {
                setComposeDirty(false);
                setView({ kind: 'read' });
              }}
              onSent={() => {
                setComposeDirty(false);
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
              onExpand={
                openId
                  ? () => {
                      // Manual per-message toggle: flip this reader AND save the mode so the next
                      // compose remembers it — but a fresh selection still opens in the side view.
                      const next = !readerExpanded;
                      setReaderExpanded(next);
                      setExpandPref(next);
                    }
                  : undefined
              }
              expanded={expanded}
            />
          )}
        </Box>
      </Box>

      {expanded && <Box className="fixed inset-0 z-40 bg-black/60" onClick={collapse} />}

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
