import { useState } from 'react';
import {
  Badge,
  Button,
  Divider,
  Field,
  GlobeIcon,
  IconButton,
  Input,
  Modal,
  Panel,
  Spinner,
  Stack,
  Text,
  TrashIcon,
  useLiveQuery,
  type ServiceApiClient,
  type ServiceUiBridge,
} from '@holistic/ui';
import type { AliasesResp, DomainsResp } from './types';

export function AdminPanel({
  api,
  ui,
  onClose,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  onClose: () => void;
}) {
  const domains = useLiveQuery<DomainsResp>(() => api.get<DomainsResp>('admin/domains'), 30000);
  const aliases = useLiveQuery<AliasesResp>(() => api.get<AliasesResp>('admin/aliases'), 30000);

  const [newDomain, setNewDomain] = useState('');
  const [aliasAddr, setAliasAddr] = useState('');
  const [aliasUser, setAliasUser] = useState('');

  async function run(fn: () => Promise<unknown>, ok: string, refresh: () => void) {
    try {
      await fn();
      ui.toast({ title: ok });
      refresh();
    } catch (e) {
      ui.toast({ title: 'Failed', description: (e as Error).message, variant: 'error' });
    }
  }

  async function removeDomain(name: string) {
    if (!(await ui.confirm({ title: `Remove domain ${name}?`, description: 'Its aliases are removed too.', danger: true, confirmLabel: 'Remove' }))) return;
    run(() => api.post('admin/domains/delete', { name }), 'Domain removed', domains.refresh);
  }

  const domainList = domains.data?.domains ?? [];
  const aliasList = aliases.data?.aliases ?? [];

  return (
    <Modal open onOpenChange={(o) => !o && onClose()} title="Mail administration" size="xl" footer={<Button variant="primary" onClick={onClose}>Done</Button>}>
      <Stack gap={5}>
        {/* Domains */}
        <Stack gap={3}>
          <Text variant="title3" weight="semibold">
            Domains
          </Text>
          <Stack direction="row" gap={2} align="end">
            <Field label="Add a sending domain" className="flex-1">
              <Input value={newDomain} onChange={(e) => setNewDomain(e.target.value)} placeholder="example.org" />
            </Field>
            <Button
              variant="primary"
              onClick={() =>
                run(() => api.post('admin/domains', { name: newDomain.trim() }), 'Domain added', () => {
                  setNewDomain('');
                  domains.refresh();
                })
              }
            >
              Add
            </Button>
          </Stack>
          {domains.loading && !domains.data ? (
            <Spinner />
          ) : (
            <Stack gap={2}>
              {domainList.map((d) => (
                <Panel key={d.name} elevation={1} className="px-3 py-2">
                  <Stack direction="row" align="center" justify="between" gap={3}>
                    <Stack direction="row" align="center" gap={2} className="min-w-0">
                      <GlobeIcon className="h-4 w-4 text-text-secondary" />
                      <Text truncate weight="medium">
                        {d.name}
                      </Text>
                      {d.primary && <Badge variant="accent">primary</Badge>}
                      <Badge variant={d.status === 'verified' ? 'success' : 'warning'}>{d.status}</Badge>
                    </Stack>
                    {!d.primary && (
                      <IconButton label="Remove domain" size="sm" variant="ghost" onClick={() => removeDomain(d.name)}>
                        <TrashIcon className="h-4 w-4" />
                      </IconButton>
                    )}
                  </Stack>
                </Panel>
              ))}
            </Stack>
          )}
        </Stack>

        <Divider />

        {/* Aliases */}
        <Stack gap={3}>
          <Text variant="title3" weight="semibold">
            Aliases
          </Text>
          <Text variant="footnote" color="tertiary">
            Route an address to a user&apos;s mailbox. One mailbox can be reached at many addresses.
          </Text>
          <Stack direction="row" gap={2} align="end" wrap>
            <Field label="Address" className="flex-1">
              <Input value={aliasAddr} onChange={(e) => setAliasAddr(e.target.value)} placeholder="info@example.org" />
            </Field>
            <Field label="Delivers to user" className="flex-1">
              <Input value={aliasUser} onChange={(e) => setAliasUser(e.target.value)} placeholder="username" />
            </Field>
            <Button
              variant="primary"
              onClick={() =>
                run(() => api.post('admin/aliases', { address: aliasAddr.trim(), user: aliasUser.trim() }), 'Alias added', () => {
                  setAliasAddr('');
                  setAliasUser('');
                  aliases.refresh();
                })
              }
            >
              Add
            </Button>
          </Stack>
          {aliases.loading && !aliases.data ? (
            <Spinner />
          ) : (
            <Stack gap={2}>
              {aliasList.map((a) => (
                <Stack key={a.address} direction="row" align="center" justify="between" gap={3} className="rounded-lg border border-separator px-3 py-2">
                  <Text truncate>
                    {a.address} → <Text as="span" weight="semibold">{a.user}</Text>
                  </Text>
                  <IconButton
                    label="Remove alias"
                    size="sm"
                    variant="ghost"
                    onClick={() => run(() => api.post('admin/aliases/delete', { address: a.address }), 'Alias removed', aliases.refresh)}
                  >
                    <TrashIcon className="h-4 w-4" />
                  </IconButton>
                </Stack>
              ))}
              {aliasList.length === 0 && (
                <Text variant="footnote" color="tertiary">
                  No aliases yet.
                </Text>
              )}
            </Stack>
          )}
        </Stack>
      </Stack>
    </Modal>
  );
}
