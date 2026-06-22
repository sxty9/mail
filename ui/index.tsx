import { MailIcon, type ServicePlugin } from '@holistic/ui';
import { MailApp } from './MailApp';

// The mail service's dashboard plugin. Linked into holistic/frontend/external/mail at install
// time and discovered by the host SPA's build-time registry. `id` MUST equal the link dir
// name and the permissions manifest's "service" field.
const plugin: ServicePlugin = {
  id: 'mail',
  displayName: 'Mail',
  icon: MailIcon,
  order: 30,
  Component: MailApp,
};

export default plugin;
