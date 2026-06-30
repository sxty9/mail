import { useState } from 'react';
import {
  Badge,
  Button,
  ChevronDownIcon,
  DropdownMenu,
  FileTextIcon,
  FolderIcon,
  FolderPlusIcon,
  IconButton,
  Input,
  MailIcon,
  Modal,
  PencilIcon,
  SendIcon,
  Stack,
  Text,
  TrashIcon,
  TreeNav,
  type MenuItem,
  type ServiceApiClient,
  type ServiceUiBridge,
  type TreeNavNode,
  type TreeNavPosition,
} from '@holistic/ui';
import type { FolderInfo } from './types';
import { folderDepth, folderLeaf, folderParent, isInSubtree } from './helpers';

function folderIcon(name: string) {
  switch (name) {
    case 'INBOX':
      return <MailIcon className="h-4 w-4" />;
    case 'Sent':
      return <SendIcon className="h-4 w-4" />;
    case 'Drafts':
      return <FileTextIcon className="h-4 w-4" />;
    case 'Trash':
      return <TrashIcon className="h-4 w-4" />;
    default:
      return <FolderIcon className="h-4 w-4" />;
  }
}

export function FolderSidebar({
  api,
  ui,
  folders,
  active,
  onSelect,
  onChanged,
  className,
}: {
  api: ServiceApiClient;
  ui: ServiceUiBridge;
  folders: FolderInfo[];
  active: string;
  onSelect: (name: string) => void;
  onChanged: () => void;
  className?: string;
}) {
  // creatingUnder: undefined = closed, null = new root folder, string = new subfolder of that parent.
  const [creatingUnder, setCreatingUnder] = useState<string | null | undefined>(undefined);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [name, setName] = useState('');

  const customNames = folders.filter((f) => f.custom).map((f) => f.name);
  // The header "+" nests under the selected folder when that folder is a user folder; for a
  // standard folder (Inbox/Sent/…) — which can't hold subfolders — it creates a top-level folder.
  const activeIsCustom = folders.some((f) => f.name === active && f.custom);

  async function submitCreate() {
    const leaf = name.trim();
    if (!leaf) return;
    const full = creatingUnder ? `${creatingUnder}/${leaf}` : leaf;
    try {
      await api.post('folders/create', { name: full });
      ui.toast({ title: `Folder “${leaf}” created` });
      setCreatingUnder(undefined);
      setName('');
      onChanged();
    } catch (e) {
      ui.toast({ title: 'Could not create folder', description: (e as Error).message, variant: 'error' });
    }
  }

  async function submitRename() {
    const leaf = name.trim();
    if (!leaf || !renaming) return;
    const parent = folderParent(renaming);
    const full = parent ? `${parent}/${leaf}` : leaf;
    if (full === renaming) {
      setRenaming(null);
      return;
    }
    try {
      await api.post('folders/rename', { name: renaming, newName: full });
      ui.toast({ title: 'Folder renamed' });
      if (active === renaming || isInSubtree(active, renaming)) onSelect(active.replace(renaming, full));
      setRenaming(null);
      setName('');
      onChanged();
    } catch (e) {
      ui.toast({ title: 'Could not rename folder', description: (e as Error).message, variant: 'error' });
    }
  }

  async function remove(folder: string) {
    const ok = await ui.confirm({
      title: `Delete folder “${folderLeaf(folder)}”?`,
      description: 'Its messages — and any subfolders — are moved to Trash.',
      danger: true,
      confirmLabel: 'Delete',
    });
    if (!ok) return;
    try {
      await api.post('folders/delete', { name: folder });
      ui.toast({ title: 'Folder deleted' });
      if (isInSubtree(active, folder)) onSelect('INBOX');
      onChanged();
    } catch (e) {
      ui.toast({ title: 'Could not delete folder', description: (e as Error).message, variant: 'error' });
    }
  }

  // moveByRename re-parents a folder (and its subtree) and keeps the active selection pointing at
  // the moved folder if the user was inside it. The backend rejects moving a folder into its own
  // subtree; we also guard here so an obviously-illegal drop is a no-op.
  async function moveByRename(dragId: string, newName: string, toastTitle: string) {
    if (newName === dragId || isInSubtree(newName, dragId)) return;
    await api.post('folders/rename', { name: dragId, newName });
    if (isInSubtree(active, dragId)) onSelect(active.replace(dragId, newName));
    ui.toast({ title: toastTitle });
  }

  // onMove translates a drag gesture into the backend model: a same-parent before/after is a pure
  // reorder; dropping inside a folder, or before/after a folder in a different branch, re-parents.
  async function onMove(dragId: string, targetId: string, pos: TreeNavPosition) {
    try {
      if (pos === 'inside') {
        if (isInSubtree(targetId, dragId)) return;
        await moveByRename(dragId, `${targetId}/${folderLeaf(dragId)}`, `Moved into “${folderLeaf(targetId)}”`);
      } else if (folderParent(targetId) === folderParent(dragId)) {
        const order = reorderWithin(customNames, dragId, targetId, pos);
        await api.post('folders/reorder', { order });
      } else {
        const newParent = folderParent(targetId);
        const newName = (newParent ? `${newParent}/` : '') + folderLeaf(dragId);
        await moveByRename(dragId, newName, newParent ? `Moved into “${folderLeaf(newParent)}”` : 'Moved to top level');
      }
      onChanged();
    } catch (e) {
      ui.toast({ title: 'Could not move folder', description: (e as Error).message, variant: 'error' });
    }
  }

  const nodes = buildNodes(folders, active);

  function rowActions(node: TreeNavNode) {
    if (!node.draggable) return null; // standard folders have no management menu
    const items: MenuItem[] = [
      {
        id: 'sub',
        label: 'New subfolder',
        icon: <FolderPlusIcon className="h-4 w-4" />,
        onSelect: () => {
          setCreatingUnder(node.id);
          setName('');
        },
      },
      {
        id: 'rename',
        label: 'Rename',
        icon: <PencilIcon className="h-4 w-4" />,
        onSelect: () => {
          setRenaming(node.id);
          setName(folderLeaf(node.id));
        },
      },
      { id: 'delete', label: 'Delete', icon: <TrashIcon className="h-4 w-4" />, danger: true, onSelect: () => remove(node.id) },
    ];
    return (
      <DropdownMenu
        align="end"
        items={items}
        trigger={
          <IconButton label="Folder actions" size="sm" variant="ghost">
            <ChevronDownIcon className="h-4 w-4" />
          </IconButton>
        }
      />
    );
  }

  return (
    <Stack className={className} gap={1}>
      <Stack direction="row" align="center" justify="between" className="px-1 pb-1">
        <Text variant="footnote" color="tertiary" weight="semibold">
          Folders
        </Text>
        <IconButton
          label={activeIsCustom ? `New subfolder in “${folderLeaf(active)}”` : 'New folder'}
          size="sm"
          variant="ghost"
          onClick={() => {
            setCreatingUnder(activeIsCustom ? active : null);
            setName('');
          }}
        >
          <FolderPlusIcon className="h-4 w-4" />
        </IconButton>
      </Stack>

      <TreeNav nodes={nodes} onSelect={onSelect} onMove={onMove} rowActions={rowActions} />

      <Modal
        open={creatingUnder !== undefined}
        onOpenChange={(o) => !o && setCreatingUnder(undefined)}
        title={creatingUnder ? `New subfolder in “${folderLeaf(creatingUnder)}”` : 'New folder'}
        size="sm"
        footer={
          <>
            <Button variant="ghost" onClick={() => setCreatingUnder(undefined)}>
              Cancel
            </Button>
            <Button variant="primary" onClick={submitCreate}>
              Create
            </Button>
          </>
        }
      >
        <Input
          autoFocus
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submitCreate()}
          placeholder="Folder name"
        />
      </Modal>

      <Modal
        open={renaming !== null}
        onOpenChange={(o) => !o && setRenaming(null)}
        title="Rename folder"
        size="sm"
        footer={
          <>
            <Button variant="ghost" onClick={() => setRenaming(null)}>
              Cancel
            </Button>
            <Button variant="primary" onClick={submitRename}>
              Rename
            </Button>
          </>
        }
      >
        <Input
          autoFocus
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submitRename()}
          placeholder="New name"
        />
      </Modal>
    </Stack>
  );
}

// buildNodes renders standard folders first (pinned, depth 0), then the custom folders as a tree:
// a DFS that keeps each level in the backend's persisted order, so parents precede their children
// and siblings honour the user's drag-reordering.
function buildNodes(folders: FolderInfo[], active: string): TreeNavNode[] {
  const out: TreeNavNode[] = [];
  const badge = (f: FolderInfo) => (f.unread > 0 ? <Badge variant="accent">{f.unread}</Badge> : undefined);

  for (const f of folders.filter((x) => !x.custom)) {
    out.push({
      id: f.name,
      label: f.name === 'INBOX' ? 'Inbox' : f.name,
      icon: folderIcon(f.name),
      depth: 0,
      active: f.name === active,
      draggable: false,
      droppable: false,
      badge: badge(f),
    });
  }

  const custom = folders.filter((x) => x.custom);
  const byName = new Map(custom.map((f) => [f.name, f]));
  const children = new Map<string, FolderInfo[]>();
  const roots: FolderInfo[] = [];
  for (const f of custom) {
    const p = folderParent(f.name);
    if (p && byName.has(p)) {
      const arr = children.get(p) ?? [];
      arr.push(f);
      children.set(p, arr);
    } else {
      roots.push(f);
    }
  }
  const visit = (f: FolderInfo) => {
    out.push({
      id: f.name,
      label: folderLeaf(f.name),
      icon: <FolderIcon className="h-4 w-4" />,
      depth: folderDepth(f.name),
      active: f.name === active,
      draggable: true,
      droppable: true,
      badge: badge(f),
    });
    (children.get(f.name) ?? []).forEach(visit);
  };
  roots.forEach(visit);
  return out;
}

// reorderWithin returns the full custom-folder order with dragId repositioned immediately
// before/after targetId. Only relative sibling order matters to the tree, so moving one entry while
// keeping the rest in place is sufficient.
function reorderWithin(names: string[], dragId: string, targetId: string, pos: TreeNavPosition): string[] {
  const without = names.filter((n) => n !== dragId);
  const at = without.indexOf(targetId);
  if (at < 0) return names;
  const insertAt = pos === 'before' ? at : at + 1;
  return [...without.slice(0, insertAt), dragId, ...without.slice(insertAt)];
}
