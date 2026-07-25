// Pure helpers shared across the mail UI. No JSX here — only data/string utilities and the
// binary-download flow (which goes through props.api.raw, never fetch).
import type { ContactOption, FileEntry, ServiceApiClient, ViewerKind } from '@holistic/ui';
import type { AttachmentView, MessageFull, OutAttachment } from './types';

/** Friendly name from a "Name <addr>" or bare address. */
export function displayName(addr: string): string {
  if (!addr) return '(unknown)';
  const m = addr.match(/^\s*"?([^"<]*)"?\s*<([^>]+)>/);
  if (m && m[1].trim()) return m[1].trim();
  const inner = m ? m[2] : addr;
  const at = inner.indexOf('@');
  return at > 0 ? inner.slice(0, at) : inner.trim();
}

/** Bare address out of "Name <addr>" or a plain address. */
export function bareAddress(addr: string): string {
  const m = addr.match(/<([^>]+)>/);
  return (m ? m[1] : addr).trim();
}

/** Compact date: time today, otherwise a short date. */
export function formatWhen(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const sameDay = d.toDateString() === new Date().toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' });
}

/** Full timestamp for the reading pane. */
export function formatFull(iso: string): string {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' });
}

export function splitAddrs(s: string): string[] {
  return s
    .split(/[,;]/)
    .map((x) => x.trim())
    .filter(Boolean);
}

/**
 * parseRecipients turns a header-style address string ("Alice <a@x>, b@y") into the ContactOption[]
 * the shared ContactPicker consumes. The email is the bare address; the chip label is the friendly
 * name when the token carried one, otherwise the address itself (matching the picker's own free-text
 * behaviour). This lets replies/forwards/drafts — which persist addresses as plain strings — hydrate
 * the picker without a separate data path.
 */
export function parseRecipients(s: string): ContactOption[] {
  return splitAddrs(s).map((token) => {
    const email = bareAddress(token);
    const named = /<[^>]+>/.test(token) ? displayName(token) : '';
    return { email, displayName: named || email };
  });
}

export function quote(text: string): string {
  return text
    .split('\n')
    .map((l) => `> ${l}`)
    .join('\n');
}

// ── folder hierarchy helpers (wire names are "/"-delimited, e.g. "Work/Projects") ──────

/** The last path segment of a folder name ("Work/Projects" → "Projects"). */
export function folderLeaf(name: string): string {
  const i = name.lastIndexOf('/');
  return i >= 0 ? name.slice(i + 1) : name;
}

/** The parent folder name, or "" for a root folder ("Work/Projects" → "Work"). */
export function folderParent(name: string): string {
  const i = name.lastIndexOf('/');
  return i >= 0 ? name.slice(0, i) : '';
}

/** Nesting depth (number of "/" separators). */
export function folderDepth(name: string): number {
  return name.split('/').length - 1;
}

/** True when `name` is `ancestor` itself or lives somewhere beneath it. */
export function isInSubtree(name: string, ancestor: string): boolean {
  return name === ancestor || name.startsWith(ancestor + '/');
}

function escapeHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

/** A friendly label for a standard mailbox, or the leaf name for a custom one. */
export function folderLabel(name: string): string {
  if (name === 'INBOX') return 'Inbox';
  return folderLeaf(name);
}

/**
 * replyDefaults builds the quoted reply body for both formats: a "> "-quoted plain text and an
 * HTML blockquote, each prefixed with an attribution line. We quote the original's PLAIN text (not
 * its raw HTML) to keep the composer clean and free of remote content.
 */
export function replyDefaults(m: MessageFull): { html: string; text: string } {
  const attribution = `On ${formatFull(m.date)}, ${displayName(m.from)} wrote:`;
  const text = `\n\n${attribution}\n${quote(m.text || '')}`;
  const quotedHtml = escapeHtml(m.text || '').replace(/\r?\n/g, '<br>');
  const html = `<p><br></p><p>${escapeHtml(attribution)}</p><blockquote>${quotedHtml}</blockquote>`;
  return { html, text };
}

/** forwardDefaults builds the forwarded body (an attribution header + the original, quoted). */
export function forwardDefaults(m: MessageFull): { html: string; text: string } {
  const head = [
    '---------- Forwarded message ----------',
    `From: ${m.from}`,
    `Date: ${formatFull(m.date)}`,
    `Subject: ${m.subject}`,
    `To: ${m.to}`,
  ];
  const text = `\n\n${head.join('\n')}\n\n${m.text || ''}`;
  const htmlHead = head.map((l) => escapeHtml(l)).join('<br>');
  const body = escapeHtml(m.text || '').replace(/\r?\n/g, '<br>');
  const html = `<p><br></p><p>${htmlHead}</p><p>${body}</p>`;
  return { html, text };
}

/** Human-readable byte size. */
export function formatSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

/** Read a File into a base64 string (no data: prefix) for the compose payload. */
export function fileToBase64(file: File): Promise<OutAttachment> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error ?? new Error('read failed'));
    reader.onload = () => {
      const result = String(reader.result ?? '');
      const comma = result.indexOf(',');
      resolve({
        filename: file.name || 'attachment',
        contentType: file.type || 'application/octet-stream',
        data: comma >= 0 ? result.slice(comma + 1) : result,
      });
    };
    reader.readAsDataURL(file);
  });
}

/**
 * viewerFor maps a MIME type to the SDK FilePreview viewer that can render it inline, or undefined
 * for types that must be downloaded. SVG/HTML are intentionally not inline-previewable (script risk;
 * the backend also refuses to serve them inline).
 */
export function viewerFor(mime: string): ViewerKind | undefined {
  const m = (mime || '').toLowerCase().split(';')[0].trim();
  if (m === 'image/svg+xml') return undefined;
  if (m.startsWith('image/')) return 'image';
  if (m === 'application/pdf') return 'pdf';
  if (m.startsWith('audio/')) return 'audio';
  if (m.startsWith('video/')) return 'video';
  if (m === 'text/markdown') return 'markdown';
  if (m === 'text/plain') return 'text';
  return undefined;
}

/** Build a synthetic SDK FileEntry for a mail attachment so the shared FilePreview can render it. */
export function attachmentEntry(a: AttachmentView): FileEntry {
  const name = a.filename || `attachment-${a.index}`;
  return { name, path: name, kind: 'file', size: a.size, mtime: 0, mime: a.contentType, viewer: viewerFor(a.contentType) ?? null };
}

/** Fetch an attachment via the authenticated api.raw channel and save it to disk. */
export async function downloadAttachment(api: ServiceApiClient, path: string, filename: string): Promise<void> {
  const res = await api.raw(path);
  if (!res.ok) throw new Error(`download failed (${res.status})`);
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename || 'attachment';
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
