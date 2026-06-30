// Shapes returned by the maild backend under /api/services/mail/.

// A folder is INBOX/Sent/Drafts/Trash or any user-created custom folder name.
export type Folder = string;

export const STD_FOLDERS = ['INBOX', 'Sent', 'Drafts', 'Trash'] as const;

export interface FolderInfo {
  name: Folder;
  total: number;
  unread: number;
  custom: boolean;
}

export interface MailboxesResp {
  address: string;
  mailDomain: string;
  folders: FolderInfo[];
}

export interface MessageMeta {
  id: string;
  folder: Folder;
  from: string;
  to: string;
  subject: string;
  date: string; // RFC3339, or "" if unknown
  seen: boolean;
  flagged: boolean;
  answered: boolean;
  hasAttachments: boolean;
  size: number;
  messageId: string;
}

export interface MessagesResp {
  folder: Folder;
  messages: MessageMeta[];
}

export interface AttachmentView {
  index: number;
  filename: string;
  contentType: string;
  size: number;
  inline: boolean;
  contentId?: string;
}

export interface MessageFull {
  id: string;
  folder: Folder;
  from: string;
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  date: string;
  messageId: string;
  inReplyTo: string;
  references: string[];
  text: string;
  html: string;
  attachments: AttachmentView[];
}

export interface SendResult {
  messageId: string;
  deliveredLocal: number;
  queuedExternal: number;
  edgeConfigured: boolean;
}

export interface Info {
  service: string;
  version: string;
  user: string;
  isAdmin: boolean;
  address: string;
  addresses: string[]; // every address the user may send from (primary first)
  mailDomain: string;
  canAdmin: boolean;
  canSend: boolean;
}

// Outgoing compose attachment (base64-encoded bytes).
export interface OutAttachment {
  filename: string;
  contentType: string;
  data: string;
}

// Admin: domains + aliases registry.
export interface Domain {
  name: string;
  primary: boolean;
  status: string; // "verified" | "pending"
  provider?: string;
  addedAt?: string;
}

export interface Alias {
  address: string;
  user: string;
}

export interface DomainsResp {
  domains: Domain[];
}

export interface AliasesResp {
  aliases: Alias[];
}

export interface AdminUserResp {
  user: string;
  address: string;
  addresses: string[];
  aliases: Alias[];
}

export interface AppPasswordMeta {
  id: string;
  label: string;
  created: string;
}

export interface AppPasswordsResp {
  username: string;
  appPasswords: AppPasswordMeta[];
}

export interface AppPasswordCreated {
  id: string;
  label: string;
  created: string;
  token: string;
  username: string;
  jmapUrl: string;
}
