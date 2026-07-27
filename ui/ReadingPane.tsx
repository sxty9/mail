import { useEffect, useState } from 'react';
import {
  Avatar,
  BoltIcon,
  Box,
  Button,
  DownloadIcon,
  DropdownMenu,
  EmptyState,
  FileIcon,
  FilePreview,
  IconButton,
  InlineLink,
  MailIcon,
  MaximizeIcon,
  MoveIcon,
  ReplyIcon,
  SafeHtmlEmail,
  SendIcon,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  formatBytes,
  userHasRight,
  type FileEntry,
  type MenuItem,
  type SessionUser,
  type ServiceApiClient,
  type ServiceUiBridge,
  type TextPayload,
} from '@holisdk/ui';
import type { AttachmentView, FolderInfo, MessageFull } from './types';
import { attachmentEntry, displayName, downloadAttachment, folderLabel, formatFull } from './helpers';
import { SummarizePanel } from './SummarizePanel';

export function ReadingPane({
  api,
  apiFor,
  ui,
  user,
  openService,
  folder,
  id,
  folders,
  onChanged,
  onReply,
  onForward,
  onExpand,
  expanded,
  className,
}: {
  api: ServiceApiClient;
  // A client for any sibling service + the current user + tab-switch — needed so the shared
  // FilePreview can offer aigentic's "Ask AI" on an attachment (aigentic reaches its own backend).
  apiFor: (serviceId: string) => ServiceApiClient;
  ui: ServiceUiBridge;
  user: SessionUser;
  openService: (serviceId: string, subPath?: string) => void;
  folder: string;
  id: string | null;
  folders: FolderInfo[];
  onChanged: () => void;
  onReply?: (m: MessageFull) => void;
  onForward?: (m: MessageFull) => void;
  onExpand?: () => void;
  expanded?: boolean;
  className?: string;
}) {
  const [msg, setMsg] = useState<MessageFull | null>(null);
  const [loading, setLoading] = useState(false);
  const [plain, setPlain] = useState(false);
  const [summaryOpen, setSummaryOpen] = useState(false);
  const [preview, setPreview] = useState<{ entry: FileEntry; index: number; rawUrl?: string; text?: TextPayload | null } | null>(null);

  useEffect(() => {
    setPlain(false);
    setPreview(null);
    setSummaryOpen(false);
    if (!id) {
      setMsg(null);
      return;
    }
    let active = true;
    setLoading(true);
    setMsg(null);
    api
      .get<MessageFull>(`message?mailbox=${encodeURIComponent(folder)}&id=${encodeURIComponent(id)}`)
      .then((m) => active && setMsg(m))
      .catch(() => active && ui.toast({ title: 'Could not load message', variant: 'error' }))
      .finally(() => active && setLoading(false));
    return () => {
      active = false;
    };
  }, [api, ui, folder, id]);

  if (!id) {
    return (
      <Stack className={`flex h-full ${className ?? ''}`} align="center" justify="center">
        <EmptyState icon={<MailIcon />} title="No message selected" description="Pick a message from the list to read it." />
      </Stack>
    );
  }
  if (loading || !msg) {
    return (
      <Stack className={`flex h-full ${className ?? ''}`} align="center" justify="center">
        <Spinner />
      </Stack>
    );
  }

  async function act(path: string, body: unknown, okMsg: string) {
    try {
      await api.post(path, body);
      ui.toast({ title: okMsg });
      onChanged();
    } catch (e) {
      ui.toast({ title: 'Action failed', description: (e as Error).message, variant: 'error' });
    }
  }

  function attUrl(index: number, inline: boolean): string {
    const base = `attachment?mailbox=${encodeURIComponent(folder)}&id=${encodeURIComponent(msg?.id ?? '')}&index=${index}`;
    return inline ? `${base}&inline=1` : base;
  }

  function saveAttachment(index: number, name: string) {
    downloadAttachment(api, attUrl(index, false), name).catch((e) => ui.toast({ title: 'Download failed', description: (e as Error).message, variant: 'error' }));
  }

  // loadPreview shows attachment[a.index] in the shared SDK FilePreview: text fetched as a payload,
  // image/pdf/audio/video via an authenticated inline url, anything else as the download fallback.
  async function loadPreview(a: AttachmentView) {
    const entry = attachmentEntry(a);
    if (entry.viewer === 'text' || entry.viewer === 'markdown') {
      try {
        const res = await api.raw(attUrl(a.index, false));
        if (!res.ok) throw new Error(`download failed (${res.status})`);
        let content = await res.text();
        let truncated = false;
        if (content.length > 200_000) {
          content = content.slice(0, 200_000);
          truncated = true;
        }
        setPreview({ entry, index: a.index, text: { content, truncated } });
      } catch {
        setPreview({ entry, index: a.index, text: null });
      }
      return;
    }
    if (entry.viewer) {
      setPreview({ entry, index: a.index, rawUrl: api.url(attUrl(a.index, true)) });
      return;
    }
    setPreview({ entry, index: a.index });
  }

  // A chip click previews viewable types and downloads the rest; prev/next inside the viewer cycles
  // through every attachment (non-viewable ones show the viewer's download fallback).
  function openAttachment(a: AttachmentView) {
    if (attachmentEntry(a).viewer) {
      void loadPreview(a);
    } else {
      saveAttachment(a.index, a.filename || `attachment-${a.index}`);
    }
  }

  const moveTargets: MenuItem[] = folders
    .filter((f) => f.name !== folder)
    .map((f) => ({
      id: f.name,
      label: folderLabel(f.name),
      onSelect: () => act('move', { mailbox: folder, id, to: f.name }, `Moved to ${folderLabel(f.name)}`),
    }));

  const hasHtml = !!msg.html && !plain;

  // "Summarize with AI" reaches the sibling aigentic service. Only offer it to users who may call it
  // (hp_aigentic_run — also enforced there), and send the engine kind they're entitled to: the paid
  // Auto router ('choose') when they hold the metered cost right, otherwise the non-metered Claude CLI.
  const canSummarize = userHasRight(user, 'hp_aigentic_run');
  const summaryKind = userHasRight(user, 'hp_aigentic_api') ? 'choose' : 'claude-cli';

  return (
    <Stack gap={0} className={`flex h-full min-h-0 flex-col ${className ?? ''}`}>
      <Stack gap={3} className="shrink-0 border-b border-separator px-5 py-4">
        <Stack direction="row" align="start" justify="between" gap={2}>
          <Text variant="title3" weight="semibold" className="min-w-0">
            {msg.subject || '(no subject)'}
          </Text>
          {onExpand && (
            <IconButton label={expanded ? 'Shrink' : 'Open in a larger window'} variant="ghost" size="sm" onClick={onExpand} className="shrink-0">
              <MaximizeIcon className="h-4 w-4" />
            </IconButton>
          )}
        </Stack>
        <Stack direction="row" align="center" gap={3} justify="between" wrap>
          <Stack direction="row" align="center" gap={3} className="min-w-0">
            <Avatar name={displayName(msg.from)} size={40} />
            <Stack gap={0} className="min-w-0">
              <Text weight="semibold" truncate>
                {displayName(msg.from)}
              </Text>
              <Text variant="footnote" color="secondary" truncate>
                {msg.from}
              </Text>
            </Stack>
          </Stack>
          <Text variant="footnote" color="tertiary">
            {formatFull(msg.date)}
          </Text>
        </Stack>
        <Text variant="footnote" color="tertiary">
          To: {msg.to || '—'}
          {msg.cc ? ` · Cc: ${msg.cc}` : ''}
        </Text>

        <Stack direction="row" gap={2} wrap align="center">
          {onReply && (
            <Button variant="primary" size="sm" iconLeft={<ReplyIcon />} onClick={() => onReply(msg)}>
              Reply
            </Button>
          )}
          {onForward && (
            <Button variant="secondary" size="sm" iconLeft={<SendIcon />} onClick={() => onForward(msg)}>
              Forward
            </Button>
          )}
          {moveTargets.length > 0 && (
            <DropdownMenu
              align="start"
              items={moveTargets}
              trigger={
                <Button variant="secondary" size="sm" iconLeft={<MoveIcon />}>
                  Move
                </Button>
              }
            />
          )}
          <Button
            variant="secondary"
            size="sm"
            iconLeft={<TrashIcon />}
            onClick={() => act('delete', { mailbox: folder, id }, 'Moved to Trash')}
          >
            Delete
          </Button>
          {canSummarize && (
            <Button
              variant="secondary"
              size="sm"
              iconLeft={<BoltIcon />}
              aria-pressed={summaryOpen}
              onClick={() => setSummaryOpen((o) => !o)}
            >
              {summaryOpen ? 'Hide AI summary' : 'Summarize with AI'}
            </Button>
          )}
          {msg.html && (
            <InlineLink onClick={() => setPlain((p) => !p)}>{plain ? 'Show formatted' : 'Show plain text'}</InlineLink>
          )}
        </Stack>

        {msg.attachments.length > 0 && (
          <Stack gap={2}>
            <Text variant="footnote" color="tertiary" weight="semibold">
              {msg.attachments.length} attachment{msg.attachments.length > 1 ? 's' : ''}
            </Text>
            <Stack direction="row" gap={2} wrap>
              {msg.attachments.map((a) => (
                <Box
                  key={a.index}
                  role="button"
                  tabIndex={0}
                  onClick={() => openAttachment(a)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      openAttachment(a);
                    }
                  }}
                  className="flex cursor-pointer items-center gap-2 rounded-lg border border-separator px-3 py-2 transition-colors hover:bg-fill/5"
                >
                  <FileIcon className="h-4 w-4 text-text-secondary" />
                  <Stack gap={0} className="min-w-0">
                    <Text variant="footnote" truncate>
                      {a.filename || `attachment-${a.index}`}
                    </Text>
                    <Text variant="caption" color="tertiary">
                      {formatBytes(a.size)}
                    </Text>
                  </Stack>
                  <IconButton
                    label="Download"
                    size="sm"
                    variant="ghost"
                    onClick={(e) => {
                      e.stopPropagation();
                      saveAttachment(a.index, a.filename || `attachment-${a.index}`);
                    }}
                  >
                    <DownloadIcon className="h-4 w-4" />
                  </IconButton>
                </Box>
              ))}
            </Stack>
          </Stack>
        )}
      </Stack>

      {canSummarize && (
        <SummarizePanel
          key={msg.id}
          open={summaryOpen}
          msg={msg}
          kind={summaryKind}
          apiFor={apiFor}
          ui={ui}
          openService={openService}
          onClose={() => setSummaryOpen(false)}
        />
      )}

      <Box className="min-h-0 flex-1 overflow-auto px-5 py-4">
        {hasHtml ? (
          <SafeHtmlEmail
            fill
            html={msg.html}
            imagesBlockedLabel="Remote images are blocked to protect your privacy."
            showImagesLabel="Show images"
          />
        ) : (
          <Text className="whitespace-pre-wrap">{msg.text || '(empty message)'}</Text>
        )}
      </Box>

      <FilePreview
        open={!!preview}
        entry={preview?.entry ?? null}
        rawUrl={preview?.rawUrl}
        text={preview?.text ?? null}
        onOpenChange={(o) => !o && setPreview(null)}
        onDownload={(e) => preview && saveAttachment(preview.index, e.name)}
        onPrev={preview && preview.index > 0 ? () => loadPreview(msg.attachments[preview.index - 1]) : undefined}
        onNext={preview && preview.index < msg.attachments.length - 1 ? () => loadPreview(msg.attachments[preview.index + 1]) : undefined}
        actionHost={{
          apiFor,
          ui,
          user,
          openService,
          // Raw bytes of the shown attachment via our authenticated channel — the SDK hands these to
          // a viewer action (aigentic's "Ask AI") for image/PDF; text rides in the preview payload.
          loadBytes: preview
            ? async () => {
                const res = await api.raw(attUrl(preview.index, false));
                if (!res.ok) throw new Error(`download failed (${res.status})`);
                return new Uint8Array(await res.arrayBuffer());
              }
            : undefined,
        }}
      />
    </Stack>
  );
}
