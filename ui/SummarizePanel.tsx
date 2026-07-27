import { useEffect, useRef, useState } from 'react';
import {
  Badge,
  BoltIcon,
  Box,
  Button,
  IconButton,
  Markdown,
  Panel,
  Spinner,
  Stack,
  Text,
  XIcon,
  type ServiceApiClient,
  type ServiceUiBridge,
} from '@holisdk/ui';
import type { MessageFull } from './types';

// The "Summarize with AI" panel that drops in under the reading-pane toolbar. It hands the open
// message's text to the aigentic service (a deliberate cross-service call via apiFor) and renders
// the Markdown summary it returns.
//
// The aigentic /run contract is mirrored locally rather than imported: the mail UI may depend only
// on @holisdk/ui and react (holistic's import allowlist), never on the aigentic package. We post a
// single text prompt; the caller supplies the engine `kind` it's entitled to (see ReadingPane —
// 'choose' for users with the paid right, else the non-metered 'claude-cli'). See aigentic
// ui/types.ts for the shared shapes and ui/AskAiFilePanel.tsx for the reference call.

// Where aigentic's chat tab looks for a seeded exchange when we hand off (aigentic ui/types.ts).
const CHAT_SEED_KEY = 'aigentic.chat.seed';
// Keep the request sane: a long thread can be huge, and Anthropic caps a request at ~32 MB. When we
// clip here we tell the user (the "input shortened" badge), so a partial summary is never silent.
const MAX_BODY_CHARS = 100_000;

interface AigenticResult {
  output: string;
  engine?: string;
  model?: string;
  usage?: { truncated?: boolean };
}
interface RunResponse {
  header: { kind: string; id?: string };
  data: AigenticResult;
}

// messageToText prefers the plain-text part; for HTML-only mail it extracts the visible text so the
// model summarizes prose, not markup. <style>/<script>/<noscript> are dropped first — their raw
// CSS/JS is text content (textContent would emit it verbatim), not something the parser strips.
function messageToText(msg: MessageFull): { body: string; truncated: boolean } {
  let body = msg.text?.trim() ?? '';
  if (!body && msg.html) {
    try {
      const doc = new DOMParser().parseFromString(msg.html, 'text/html');
      doc.body?.querySelectorAll('style, script, noscript').forEach((n) => n.remove());
      body = doc.body?.textContent?.trim() ?? '';
    } catch {
      body = '';
    }
  }
  body = body.replace(/\n{3,}/g, '\n\n');
  if (body.length > MAX_BODY_CHARS) return { body: body.slice(0, MAX_BODY_CHARS), truncated: true };
  return { body, truncated: false };
}

// buildPrompt frames the message for the model: a short instruction plus the headers and body. It
// carries the `truncated` flag through so the panel can flag a clipped input.
function buildPrompt(msg: MessageFull): { prompt: string; truncated: boolean } {
  const { body, truncated } = messageToText(msg);
  const prompt = [
    'Summarize the following email for a busy reader.',
    'Open with a one-line TL;DR, then bullet the key points and list any questions or action items.',
    'Keep it tight; skip greetings, signatures and quoted history.',
    '',
    `Subject: ${msg.subject || '(no subject)'}`,
    `From: ${msg.from}`,
    msg.to ? `To: ${msg.to}` : '',
    '',
    body || '(this message has no readable text)',
  ]
    .filter(Boolean)
    .join('\n');
  return { prompt, truncated };
}

// cleanOutput strips the context tags the backend may wrap content in (mirrors aigentic's helper).
function cleanOutput(s: string): string {
  return s
    .replace(/<\/?(file|attachment)\b[^>]*>/g, '')
    .replace(/\n{3,}/g, '\n\n')
    .trim();
}

export function SummarizePanel({
  open,
  msg,
  kind,
  apiFor,
  ui,
  openService,
  onClose,
}: {
  open: boolean;
  msg: MessageFull;
  // The aigentic engine kind the current user is entitled to (resolved from their rights upstream).
  kind: string;
  apiFor: (serviceId: string) => ServiceApiClient;
  ui: ServiceUiBridge;
  openService: (serviceId: string, subPath?: string) => void;
  onClose: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<AigenticResult | null>(null);
  const [error, setError] = useState('');
  const [inputShortened, setInputShortened] = useState(false);
  const started = useRef(false);

  const answer = result ? cleanOutput(result.output) : '';

  async function run() {
    setBusy(true);
    setError('');
    setResult(null);
    try {
      const { prompt, truncated } = buildPrompt(msg);
      setInputShortened(truncated);
      const res = await apiFor('aigentic').post<RunResponse>('run', { header: { kind }, data: { prompt } });
      setResult(res.data);
    } catch (e) {
      const detail = (e as Error).message || 'The AI request failed.';
      setError(detail);
      ui.toast({ title: 'Could not summarize', description: detail, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  useEffect(() => {
    // run() closes over this mount's message, which is stable (the panel is keyed by message id).
    if (open && !started.current) {
      started.current = true;
      void run();
    }
  }, [open]);

  if (!open) return null;

  // Hand the exchange to the full aigentic chat tab so the user can keep asking about this message.
  function continueInChat() {
    if (!result) return;
    try {
      localStorage.setItem(CHAT_SEED_KEY, JSON.stringify({ prompt: buildPrompt(msg).prompt, answer, engine: result.engine, model: result.model }));
    } catch {
      // localStorage unavailable (private mode / quota) — fall back to opening an empty chat.
    }
    openService('aigentic');
  }

  return (
    <Box className="max-h-[40%] shrink-0 overflow-auto border-b border-separator px-5 py-4">
      <Stack gap={3}>
        <Stack direction="row" align="center" justify="between" gap={2} wrap>
          <Stack direction="row" align="center" gap={2} wrap>
            <BoltIcon className="h-4 w-4 text-accent" />
            <Text weight="semibold">AI summary</Text>
            {result?.engine && <Badge variant="accent">{result.engine}</Badge>}
            {result?.model && (
              <Text variant="footnote" color="secondary">
                {result.model}
              </Text>
            )}
            {inputShortened && <Badge variant="neutral">input shortened</Badge>}
            {result?.usage?.truncated && <Badge variant="neutral">context truncated</Badge>}
          </Stack>
          <Stack direction="row" align="center" gap={1}>
            <Button variant="secondary" size="sm" loading={busy} onClick={run}>
              Regenerate
            </Button>
            <IconButton label="Hide summary" size="sm" variant="ghost" onClick={onClose}>
              <XIcon className="h-4 w-4" />
            </IconButton>
          </Stack>
        </Stack>

        <Box role="status" aria-live="polite">
          {busy ? (
            <Stack direction="row" align="center" gap={2}>
              <Spinner />
              <Text variant="footnote" color="secondary">
                Summarizing this message…
              </Text>
            </Stack>
          ) : error ? (
            <Text variant="footnote" color="danger">
              Could not summarize: {error}
            </Text>
          ) : answer ? (
            <Stack gap={3}>
              <Panel className="bg-fill/5 p-4">
                <Markdown text={answer} />
              </Panel>
              <Stack direction="row" align="center" gap={2}>
                <Button variant="secondary" size="sm" onClick={continueInChat}>
                  Continue in chat →
                </Button>
              </Stack>
            </Stack>
          ) : (
            <Text variant="footnote" color="secondary">
              (empty response)
            </Text>
          )}
        </Box>
      </Stack>
    </Box>
  );
}
