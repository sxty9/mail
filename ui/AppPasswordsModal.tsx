import { useState } from 'react';
import {
  Button,
  Divider,
  Field,
  Input,
  Modal,
  Panel,
  Stack,
  Text,
  TrashIcon,
  useLiveQuery,
  type InstanceInfo,
  type ServiceApiClient,
  type ServiceUiBridge,
} from '@holisdk/ui';
import type { AppPasswordCreated, AppPasswordsResp } from './types';

export function AppPasswordsModal({
  api,
  ui,
  instance,
  onClose,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  instance: InstanceInfo;
  onClose: () => void;
}) {
  const apps = useLiveQuery<AppPasswordsResp>(() => api.get<AppPasswordsResp>('apppasswords'), 30000);
  const [label, setLabel] = useState('');
  const [created, setCreated] = useState<AppPasswordCreated | null>(null);
  const [busy, setBusy] = useState(false);

  const jmapUrl = `${instance.origin || ''}/api/services/mail/jmap/session`;

  async function create() {
    setBusy(true);
    try {
      const res = await api.post<AppPasswordCreated>('apppasswords', { label: label.trim() || 'Mail app' });
      setCreated(res);
      setLabel('');
      apps.refresh();
    } catch (e) {
      ui.toast({ title: 'Could not create app password', description: (e as Error).message, variant: 'error' });
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: string) {
    if (!(await ui.confirm({ title: 'Revoke this app password?', danger: true, confirmLabel: 'Revoke' }))) return;
    try {
      await api.post('apppasswords/delete', { id });
      apps.refresh();
    } catch (e) {
      ui.toast({ title: 'Revoke failed', description: (e as Error).message, variant: 'error' });
    }
  }

  const list = apps.data?.appPasswords ?? [];

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title="Mail apps (JMAP)"
      size="lg"
      footer={
        <Button variant="primary" onClick={onClose}>
          Done
        </Button>
      }
    >
      <Stack gap={4}>
        <Stack gap={1}>
          <Text variant="footnote" color="tertiary">
            JMAP URL
          </Text>
          <Text className="break-all">{jmapUrl}</Text>
          <Text variant="footnote" color="tertiary">
            Username
          </Text>
          <Text>{apps.data?.username || ''}</Text>
        </Stack>
        <Divider />
        <Stack direction="row" gap={2} align="end">
          <Field label="New app password" className="flex-1">
            <Input value={label} onChange={(e) => setLabel(e.target.value)} placeholder="e.g. Laptop aerc" />
          </Field>
          <Button variant="primary" loading={busy} onClick={create}>
            Create
          </Button>
        </Stack>
        {created && (
          <Panel elevation={1} className="p-3">
            <Stack gap={1}>
              <Text weight="semibold">Copy this password now — it is shown only once:</Text>
              <Text className="break-all" color="accent">
                {created.token}
              </Text>
            </Stack>
          </Panel>
        )}
        <Stack gap={2}>
          {list.map((p) => (
            <Stack key={p.id} direction="row" align="center" justify="between" gap={2}>
              <Text>{p.label}</Text>
              <Button size="sm" variant="ghost" iconLeft={<TrashIcon />} onClick={() => revoke(p.id)}>
                Revoke
              </Button>
            </Stack>
          ))}
          {list.length === 0 && (
            <Text variant="footnote" color="tertiary">
              No app passwords yet.
            </Text>
          )}
        </Stack>
      </Stack>
    </Modal>
  );
}
