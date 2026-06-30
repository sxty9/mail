import { useEffect, useState } from 'react';
import {
  Avatar,
  Box,
  Button,
  EmptyState,
  GlobeIcon,
  KeyIcon,
  MailIcon,
  PencilIcon,
  SearchField,
  Spinner,
  Stack,
  Text,
  useLiveQuery,
  userHasRight,
  type ServiceApiClient,
  type ServiceContextProps,
} from '@holistic/ui';
import type { Info, MailboxesResp, MessageFull, MessageMeta, MessagesResp } from './types';
import { forwardDefaults, replyDefaults } from './helpers';
import { FolderSidebar } from './FolderSidebar';
import { MessageList } from './MessageList';
import { ReadingPane } from './ReadingPane';
import { Composer, type ComposeState } from './Composer';
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

  // guardLeave confirms before abandoning an unsaved compose draft (the inline composer occupies
  // the same pane used for reading, so opening a message or another compose would replace it).
  async function guardLeave(): Promise<boolean> {
    if (view.kind !== 'compose' || !composeDirty) return true;
    return ui.confirm({ title: 'Discard this message?', description: 'Your draft will be lost.', danger: true, confirmLabel: 'Discard' });
  }

  async function selectFolder(f: string) {
    if (!(await guardLeave())) return;
    setFolder(f);
    setOpenId(null);
    setView({ kind: 'read' });
    setComposeDirty(false);
  }

  async function openMessage(m: MessageMeta) {
    if (!(await guardLeave())) return;
    setOpenId(m.id);
    setView({ kind: 'read' });
    setComposeDirty(false);
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

  function startCompose(state: ComposeState) {
    setComposeSeq((n) => n + 1);
    setComposeDirty(false);
    setView({ kind: 'compose', state, seq: composeSeq + 1 });
  }

  async function compose(make: () => ComposeState) {
    if (!(await guardLeave())) return;
    startCompose(make());
  }

  function newMessage() {
    compose(() => ({ from: addresses[0], to: '', cc: '', subject: '', html: '', text: '' }));
  }

  function startReply(full: MessageFull) {
    const { html, text } = replyDefaults(full);
    startCompose({
      from: addresses[0],
      to: full.from,
      cc: '',
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
      subject: /^fwd:/i.test(full.subject) ? full.subject : `Fwd: ${full.subject}`,
      html,
      text,
    });
  }

  function refreshAll() {
    list?.refresh();
    boxes?.refresh();
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
          />
        </Box>

        <Box className="w-[340px] shrink-0 overflow-auto border-r border-separator">
          {list?.loading && !list?.data ? (
            <Stack align="center" className="py-16">
              <Spinner />
            </Stack>
          ) : (
            <MessageList messages={rows} activeId={openId} showRecipient={showRecipient} query={q} onOpen={openMessage} />
          )}
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
