// Shapes returned by the maild backend under /api/services/mail/.

export type Folder = 'INBOX' | 'Sent' | 'Drafts' | 'Trash';

export interface FolderInfo {
  name: Folder;
  total: number;
  unread: number;
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
  size: number;
  messageId: string;
}

export interface MessagesResp {
  folder: Folder;
  messages: MessageMeta[];
}

export interface MessageFull {
  id: string;
  folder: Folder;
  from: string;
  to: string;
  cc: string;
  subject: string;
  date: string;
  messageId: string;
  inReplyTo: string;
  references: string[];
  text: string;
  html: string;
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
  mailDomain: string;
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
