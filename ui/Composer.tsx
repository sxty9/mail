import { useEffect, useState } from 'react';
import {
  Box,
  Button,
  ChevronDownIcon,
  DropdownMenu,
  FileIcon,
  IconButton,
  Input,
  RichTextEditor,
  SendIcon,
  Stack,
  Text,
  UploadControl,
  XIcon,
  type MenuItem,
  type ServiceApiClient,
  type ServiceUiBridge,
} from '@holistic/ui';
import type { SendResult } from './types';
import { fileToBase64, formatSize, splitAddrs } from './helpers';

export interface ComposeState {
  from?: string;
  to: string;
  cc: string;
  subject: string;
  html: string;
  text: string;
  inReplyTo?: string;
  references?: string[];
}

/**
 * Composer is the inline compose/reply/forward surface that lives in the reading pane (no modal).
 * It fills the pane as a flex column: header + address fields are fixed at the top, the rich-text
 * editor flexes and scrolls, and the action bar (Attach · Discard · Send) is pinned at the bottom
 * so Send is always reachable — the bug the modal had.
 */
export function Composer({
  api,
  ui,
  state,
  addresses,
  onClose,
  onSent,
  onDirtyChange,
  className,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  state: ComposeState;
  addresses: string[];
  onClose: () => void;
  onSent: () => void;
  onDirtyChange?: (dirty: boolean) => void;
  className?: string;
}) {
  const [from, setFrom] = useState(state.from || addresses[0] || '');
  const [to, setTo] = useState(state.to);
  const [cc, setCc] = useState(state.cc);
  const [showCc, setShowCc] = useState(!!state.cc);
  const [subject, setSubject] = useState(state.subject);
  const [html, setHtml] = useState(state.html);
  const [text, setText] = useState(state.text);
  const [files, setFiles] = useState<File[]>([]);
  const [busy, setBusy] = useState(false);

  // Report whether there's content worth protecting, so the host can confirm before discarding it.
  const dirty = !!(to.trim() || cc.trim() || subject.trim() || text.trim() || files.length);
  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  const fromItems: MenuItem[] = addresses.map((a) => ({ id: a, label: a, checked: a === from, onSelect: () => setFrom(a) }));

  async function send() {
    if (!to.trim() && !cc.trim()) {
      ui.toast({ title: 'Add at least one recipient', variant: 'error' });
      return;
    }
    setBusy(true);
    try {
      const attachments = await Promise.all(files.map((f) => fileToBase64(f)));
      const res = await api.post<SendResult>('send', {
        from: from || undefined,
        to: splitAddrs(to),
        cc: splitAddrs(cc),
        subject,
        body: text,
        htmlBody: html,
        inReplyTo: state.inReplyTo,
        references: state.references,
        attachments,
      });
      const queued =
        res.queuedExternal > 0 && !res.edgeConfigured ? ' — external recipients queued (mail edge not configured yet)' : '';
      ui.toast({ title: `Message sent${queued}`, variant: 'success' });
      onSent();
    } catch (e) {
      ui.toast({ title: 'Send failed', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function discard() {
    if (text.trim() || subject.trim() || to.trim() || files.length) {
      const ok = await ui.confirm({ title: 'Discard this message?', danger: true, confirmLabel: 'Discard' });
      if (!ok) return;
    }
    onClose();
  }

  return (
    <Stack gap={0} className={`flex h-full min-h-0 flex-col ${className ?? ''}`}>
      <Stack direction="row" align="center" justify="between" gap={2} className="shrink-0 border-b border-separator px-4 py-3">
        <Text variant="headline" weight="semibold" truncate>
          {subject.trim() || 'New message'}
        </Text>
        <IconButton label="Close" variant="ghost" size="sm" onClick={discard}>
          <XIcon className="h-4 w-4" />
        </IconButton>
      </Stack>

      <Stack gap={2} className="shrink-0 border-b border-separator px-4 py-3">
        <Stack direction="row" align="center" gap={2}>
          <Text variant="footnote" color="tertiary" className="w-12 shrink-0">
            From
          </Text>
          {addresses.length > 1 ? (
            <DropdownMenu
              align="start"
              items={fromItems}
              trigger={
                <Button variant="secondary" size="sm" iconRight={<ChevronDownIcon />}>
                  {from || addresses[0]}
                </Button>
              }
            />
          ) : (
            <Text variant="footnote" color="secondary">
              {addresses[0]}
            </Text>
          )}
        </Stack>
        <Stack direction="row" align="center" gap={2}>
          <Text variant="footnote" color="tertiary" className="w-12 shrink-0">
            To
          </Text>
          <Input value={to} onChange={(e) => setTo(e.target.value)} placeholder="user@domain, …" className="flex-1" />
          {!showCc && (
            <Button variant="ghost" size="sm" onClick={() => setShowCc(true)}>
              Cc
            </Button>
          )}
        </Stack>
        {showCc && (
          <Stack direction="row" align="center" gap={2}>
            <Text variant="footnote" color="tertiary" className="w-12 shrink-0">
              Cc
            </Text>
            <Input value={cc} onChange={(e) => setCc(e.target.value)} placeholder="user@domain, …" className="flex-1" />
          </Stack>
        )}
        <Stack direction="row" align="center" gap={2}>
          <Text variant="footnote" color="tertiary" className="w-12 shrink-0">
            Subject
          </Text>
          <Input value={subject} onChange={(e) => setSubject(e.target.value)} className="flex-1" />
        </Stack>
      </Stack>

      <Box className="min-h-0 flex-1 px-4 py-3">
        <RichTextEditor
          className="h-full"
          defaultValue={state.html}
          autoFocus
          placeholder="Write your message…"
          onChange={(h, t) => {
            setHtml(h);
            setText(t);
          }}
        />
      </Box>

      {files.length > 0 && (
        <Stack gap={2} className="max-h-28 shrink-0 overflow-auto border-t border-separator px-4 py-3">
          <Stack direction="row" gap={2} wrap>
            {files.map((f, i) => (
              <Stack
                key={`${f.name}-${i}`}
                direction="row"
                align="center"
                gap={2}
                className="rounded-lg border border-separator px-3 py-1.5"
              >
                <FileIcon className="h-4 w-4 text-text-secondary" />
                <Stack gap={0} className="min-w-0">
                  <Text variant="footnote" truncate>
                    {f.name}
                  </Text>
                  <Text variant="caption" color="tertiary">
                    {formatSize(f.size)}
                  </Text>
                </Stack>
                <IconButton
                  label="Remove attachment"
                  size="sm"
                  variant="ghost"
                  onClick={() => setFiles((cur) => cur.filter((_, j) => j !== i))}
                >
                  <XIcon className="h-4 w-4" />
                </IconButton>
              </Stack>
            ))}
          </Stack>
        </Stack>
      )}

      <Stack direction="row" align="center" justify="between" gap={2} className="shrink-0 border-t border-separator px-4 py-3">
        <UploadControl variant="secondary" label="Attach" onFiles={(f) => setFiles((cur) => [...cur, ...f])} />
        <Stack direction="row" align="center" gap={2}>
          <Button variant="ghost" onClick={discard}>
            Discard
          </Button>
          <Button variant="primary" iconLeft={<SendIcon />} loading={busy} onClick={send}>
            Send
          </Button>
        </Stack>
      </Stack>
    </Stack>
  );
}
