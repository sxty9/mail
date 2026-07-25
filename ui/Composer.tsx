import { forwardRef, useEffect, useImperativeHandle, useRef, useState } from 'react';
import {
  Box,
  Button,
  ChevronDownIcon,
  ContactPicker,
  DropdownMenu,
  FileIcon,
  IconButton,
  Input,
  MaximizeIcon,
  RichTextEditor,
  SendIcon,
  Stack,
  Text,
  UploadControl,
  XIcon,
  type ContactOption,
  type MenuItem,
  type ServiceApiClient,
  type ServiceUiBridge,
} from '@holistic/ui';
import type { AttachmentView, SendResult } from './types';
import { fileToBase64, formatSize, parseRecipients } from './helpers';

export interface ComposeState {
  from?: string;
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  html: string;
  text: string;
  inReplyTo?: string;
  references?: string[];
  /** Set when editing an existing draft, so saving replaces it and sending removes it. */
  draftId?: string;
  /** Attachments already stored in the draft, carried over by reference (no re-upload). */
  keepAttachments?: AttachmentView[];
}

/** Imperative surface so the host can auto-save the draft when the user navigates away. */
export interface ComposerHandle {
  saveDraft: () => Promise<void>;
}

export interface ComposerProps {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  state: ComposeState;
  addresses: string[];
  /** Directory type-ahead for To/Cc/Bcc — the contax lookup (see MailApp). */
  searchContacts: (query: string) => Promise<ContactOption[]>;
  /** Expand a picked contax personal group into its member addresses. */
  expandGroup?: (groupId: string) => Promise<ContactOption[]>;
  onClose: () => void;
  onSent: () => void;
  onDirtyChange?: (dirty: boolean) => void;
  onExpand?: () => void;
  expanded?: boolean;
  className?: string;
}

/**
 * Composer is the inline compose/reply/forward surface (no modal). Address fields are pinned at
 * the top, the rich-text editor flexes/scrolls, and the action bar (Attach · Discard · Save draft ·
 * Send) is pinned at the bottom. Drafts: "Save draft" persists to the Drafts folder, navigating
 * away auto-saves (via the imperative handle), sending removes the draft, and discarding deletes it.
 */
export const Composer = forwardRef<ComposerHandle, ComposerProps>(function Composer(
  { api, ui, state, addresses, searchContacts, expandGroup, onClose, onSent, onDirtyChange, onExpand, expanded, className },
  ref,
) {
  const [from, setFrom] = useState(state.from || addresses[0] || '');
  const [to, setTo] = useState<ContactOption[]>(() => parseRecipients(state.to));
  const [cc, setCc] = useState<ContactOption[]>(() => parseRecipients(state.cc));
  const [bcc, setBcc] = useState<ContactOption[]>(() => parseRecipients(state.bcc));
  const [showCc, setShowCc] = useState(cc.length > 0);
  const [showBcc, setShowBcc] = useState(bcc.length > 0);
  const [subject, setSubject] = useState(state.subject);
  const [html, setHtml] = useState(state.html);
  const [text, setText] = useState(state.text);
  const [files, setFiles] = useState<File[]>([]);
  const [kept, setKept] = useState<AttachmentView[]>(state.keepAttachments ?? []);
  const [busy, setBusy] = useState(false);
  const draftIdRef = useRef<string | null>(state.draftId ?? null);

  // Report whether there's content worth keeping, so the host knows to auto-save on navigation.
  const dirty = !!(to.length || cc.length || bcc.length || subject.trim() || text.trim() || files.length || kept.length);
  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);

  const fromItems: MenuItem[] = addresses.map((a) => ({ id: a, label: a, checked: a === from, onSelect: () => setFrom(a) }));

  function payload() {
    return {
      from: from || undefined,
      to: to.map((c) => c.email),
      cc: cc.map((c) => c.email),
      bcc: bcc.map((c) => c.email),
      subject,
      body: text,
      htmlBody: html,
      inReplyTo: state.inReplyTo,
      references: state.references,
      keepFromDraft: kept.length && draftIdRef.current ? { id: draftIdRef.current, indices: kept.map((a) => a.index) } : undefined,
    };
  }

  async function send() {
    if (!to.length && !cc.length && !bcc.length) {
      ui.toast({ title: 'Add at least one recipient', variant: 'error' });
      return;
    }
    setBusy(true);
    try {
      const attachments = await Promise.all(files.map((f) => fileToBase64(f)));
      const res = await api.post<SendResult>('send', { ...payload(), attachments });
      const queued =
        res.queuedExternal > 0 && !res.edgeConfigured ? ' — external recipients queued (mail edge not configured yet)' : '';
      ui.toast({ title: `Message sent${queued}`, variant: 'success' });
      await dropDraft();
      onSent();
    } catch (e) {
      ui.toast({ title: 'Send failed', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  // saveDraft persists the message to Drafts, replacing any previous version. Silent mode (used by
  // auto-save-on-leave) skips the toast and does not close the composer.
  async function saveDraft(silent: boolean) {
    if (!dirty) {
      if (!silent) onClose();
      return;
    }
    try {
      const attachments = await Promise.all(files.map((f) => fileToBase64(f)));
      const res = await api.post<{ id: string; attachments: AttachmentView[] }>('drafts/save', {
        ...payload(),
        attachments,
        replaceId: draftIdRef.current || undefined,
      });
      // The new draft now holds both the kept and the newly-uploaded attachments; re-reference them
      // so a subsequent save doesn't re-upload (or drop) them.
      draftIdRef.current = res.id;
      setKept(res.attachments ?? []);
      setFiles([]);
      if (!silent) {
        ui.toast({ title: 'Draft saved' });
        onClose();
      }
    } catch (e) {
      if (!silent) ui.toast({ title: 'Could not save draft', description: (e as Error).message, variant: 'error' });
    }
  }

  async function dropDraft() {
    if (!draftIdRef.current) return;
    try {
      await api.post('drafts/delete', { id: draftIdRef.current });
    } catch {
      /* best effort */
    }
    draftIdRef.current = null;
  }

  async function discard() {
    if (dirty) {
      const ok = await ui.confirm({ title: 'Discard this message?', description: 'It will not be saved as a draft.', danger: true, confirmLabel: 'Discard' });
      if (!ok) return;
    }
    await dropDraft();
    onClose();
  }

  // Keep a stable handle that always calls the latest saveDraft (which closes over current fields).
  const latest = useRef<() => Promise<void>>(async () => {});
  latest.current = () => saveDraft(true);
  useImperativeHandle(ref, () => ({ saveDraft: () => latest.current() }), []);

  return (
    <Stack gap={0} className={`flex h-full min-h-0 flex-col ${className ?? ''}`}>
      <Stack direction="row" align="center" justify="between" gap={2} className="shrink-0 border-b border-separator px-4 py-3">
        <Text variant="title3" weight="semibold" truncate>
          {subject.trim() || 'New message'}
        </Text>
        <Stack direction="row" align="center" gap={1} className="shrink-0">
          {onExpand && (
            <IconButton label={expanded ? 'Shrink' : 'Open in a larger window'} variant="ghost" size="sm" onClick={onExpand}>
              <MaximizeIcon className="h-4 w-4" />
            </IconButton>
          )}
          <IconButton label="Close" variant="ghost" size="sm" onClick={discard}>
            <XIcon className="h-4 w-4" />
          </IconButton>
        </Stack>
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
        <Stack direction="row" align="start" gap={2}>
          <Text variant="footnote" color="tertiary" className="w-12 shrink-0 pt-2">
            To
          </Text>
          <Box className="min-w-0 flex-1">
            <ContactPicker value={to} onChange={setTo} onSearch={searchContacts} onExpandGroup={expandGroup} placeholder="Name or address …" />
          </Box>
          {!showCc && (
            <Button variant="ghost" size="sm" className="mt-1" onClick={() => setShowCc(true)}>
              Cc
            </Button>
          )}
          {!showBcc && (
            <Button variant="ghost" size="sm" className="mt-1" onClick={() => setShowBcc(true)}>
              Bcc
            </Button>
          )}
        </Stack>
        {showCc && (
          <Stack direction="row" align="start" gap={2}>
            <Text variant="footnote" color="tertiary" className="w-12 shrink-0 pt-2">
              Cc
            </Text>
            <Box className="min-w-0 flex-1">
              <ContactPicker value={cc} onChange={setCc} onSearch={searchContacts} onExpandGroup={expandGroup} placeholder="Name or address …" />
            </Box>
          </Stack>
        )}
        {showBcc && (
          <Stack direction="row" align="start" gap={2}>
            <Text variant="footnote" color="tertiary" className="w-12 shrink-0 pt-2">
              Bcc
            </Text>
            <Box className="min-w-0 flex-1">
              <ContactPicker value={bcc} onChange={setBcc} onSearch={searchContacts} onExpandGroup={expandGroup} placeholder="Name or address …" />
            </Box>
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

      {(files.length > 0 || kept.length > 0) && (
        <Stack gap={2} className="max-h-28 shrink-0 overflow-auto border-t border-separator px-4 py-3">
          <Stack direction="row" gap={2} wrap>
            {kept.map((a) => (
              <Stack
                key={`kept-${a.index}`}
                direction="row"
                align="center"
                gap={2}
                className="rounded-lg border border-separator px-3 py-1.5"
              >
                <FileIcon className="h-4 w-4 text-text-secondary" />
                <Stack gap={0} className="min-w-0">
                  <Text variant="footnote" truncate>
                    {a.filename || `attachment-${a.index}`}
                  </Text>
                  <Text variant="caption" color="tertiary">
                    {formatSize(a.size)}
                  </Text>
                </Stack>
                <IconButton
                  label="Remove attachment"
                  size="sm"
                  variant="ghost"
                  onClick={() => setKept((cur) => cur.filter((x) => x.index !== a.index))}
                >
                  <XIcon className="h-4 w-4" />
                </IconButton>
              </Stack>
            ))}
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
          <Button variant="secondary" onClick={() => saveDraft(false)}>
            Save draft
          </Button>
          <Button variant="primary" iconLeft={<SendIcon />} loading={busy} onClick={send}>
            Send
          </Button>
        </Stack>
      </Stack>
    </Stack>
  );
});
