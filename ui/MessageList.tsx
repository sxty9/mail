import { Avatar, Badge, Box, EmptyState, FileIcon, MailIcon, Stack, Text } from '@holistic/ui';
import type { MessageMeta } from './types';
import { displayName, formatWhen } from './helpers';

/**
 * MessageList renders the mailbox as rounded, selectable rows (no full-width divider lines) — the
 * same visual language as the folder tree, so the whole shell reads as one consistent rounded
 * surface. The active row gets a tinted rounded highlight; unread rows show a dot + bolder sender.
 */
export function MessageList({
  messages,
  activeId,
  showRecipient,
  query,
  onOpen,
}: {
  messages: MessageMeta[];
  activeId: string | null;
  showRecipient: boolean;
  query: string;
  onOpen: (m: MessageMeta) => void;
}) {
  if (!messages.length) {
    return (
      <Stack align="center" justify="center" className="h-full px-4">
        <EmptyState
          icon={<MailIcon />}
          title="No messages"
          description={query ? 'Nothing matches your search.' : 'This mailbox is empty.'}
        />
      </Stack>
    );
  }
  return (
    <Stack gap={0} className="p-1.5">
      {messages.map((m) => {
        const who = showRecipient ? m.to : m.from;
        const selected = m.id === activeId;
        return (
          <Box
            key={m.id}
            role="button"
            tabIndex={0}
            aria-current={selected || undefined}
            onClick={() => onOpen(m)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                onOpen(m);
              }
            }}
            className={`cursor-pointer rounded-lg px-3 py-2 transition-colors ${selected ? 'bg-accent/15' : 'hover:bg-fill/8'}`}
          >
            <Stack direction="row" align="start" justify="between" gap={2} className="min-w-0">
              <Stack direction="row" align="center" gap={2} className="min-w-0">
                <Avatar name={displayName(who)} size={30} />
                <Stack gap={0} className="min-w-0">
                  <Stack direction="row" align="center" gap={2} className="min-w-0">
                    {!m.seen && <Box className="h-2 w-2 shrink-0 rounded-full bg-accent" />}
                    <Text truncate weight={m.seen ? 'normal' : 'semibold'}>
                      {displayName(who)}
                    </Text>
                  </Stack>
                  <Text truncate variant="footnote" color={m.seen ? 'secondary' : 'primary'}>
                    {m.subject || '(no subject)'}
                  </Text>
                </Stack>
              </Stack>
              <Stack direction="column" align="end" gap={1} className="shrink-0">
                <Text variant="caption" color="tertiary" className="whitespace-nowrap">
                  {formatWhen(m.date)}
                </Text>
                {(m.hasAttachments || m.flagged) && (
                  <Stack direction="row" align="center" gap={1}>
                    {m.hasAttachments && <FileIcon className="h-3 w-3 text-text-tertiary" />}
                    {m.flagged && <Badge variant="warning">!</Badge>}
                  </Stack>
                )}
              </Stack>
            </Stack>
          </Box>
        );
      })}
    </Stack>
  );
}
