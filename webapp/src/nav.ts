import type { IconName } from './components/Icon';

export type PageId =
  | 'home'
  | 'storage'
  | 'files'
  | 'activity'
  | 'approvals'
  | 'agents'
  | 'apps'
  | 'backup'
  | 'tv'
  | 'settings';

export interface NavItem {
  id: PageId;
  label: string;
  icon: IconName;
}

/** Primary navigation, in the brief's order. */
export const NAV_ITEMS: NavItem[] = [
  { id: 'home', label: 'Home', icon: 'home' },
  { id: 'storage', label: 'Storage', icon: 'database' },
  { id: 'files', label: 'Files', icon: 'folder' },
  { id: 'activity', label: 'Activity', icon: 'activity' },
  { id: 'approvals', label: 'Approvals', icon: 'approval' },
  { id: 'agents', label: 'Agents', icon: 'cpu' },
  { id: 'apps', label: 'Apps', icon: 'grid' },
  { id: 'backup', label: 'Backup', icon: 'shield' },
  { id: 'tv', label: 'TV Mode', icon: 'tv' },
  { id: 'settings', label: 'Settings', icon: 'sliders' },
];

/** Mobile bottom navigation: exactly five anchors (brief §2). */
export const MOBILE_ANCHORS: NavItem[] = [
  { id: 'home', label: 'Home', icon: 'home' },
  { id: 'storage', label: 'Storage', icon: 'database' },
  { id: 'activity', label: 'Activity', icon: 'activity' },
  { id: 'agents', label: 'Agents', icon: 'cpu' },
];

/** Sections reachable from the mobile "More" sheet. */
export const MORE_ITEMS: NavItem[] = NAV_ITEMS.filter(
  (item) => !MOBILE_ANCHORS.some((anchor) => anchor.id === item.id),
);

export const PAGE_TITLES: Record<PageId, string> = {
  home: 'Home',
  storage: 'Storage',
  files: 'Files',
  activity: 'Activity',
  approvals: 'Approvals',
  agents: 'Agents',
  apps: 'App Library',
  backup: 'Backup',
  tv: 'TV Mode',
  settings: 'Settings',
};
