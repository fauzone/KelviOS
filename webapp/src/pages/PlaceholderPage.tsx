import type { PageId } from '../nav';

const DESCRIPTIONS: Record<Exclude<PageId, 'home'>, string> = {
  storage:
    'Pools, disks & health, folders & shares, and the Create Pool wizard — the storage depth of KelviOS, one level down from Home.',
  files:
    'Browse the whole namespace — including files on sleeping Cold disks — with upload, download, move, media previews, and share links.',
  activity:
    'Every background job in one place: running, scheduled, history, and rules & recipes — status, next run, and logs one click away.',
  approvals:
    'Decisions waiting for you: lifecycle recommendations and agent actions, each with context, risk, and expiry — approve or reject without blocking anything else.',
  agents:
    'The starter agents — Health Guardian, Backup Steward, Tier Curator, Capacity Planner — their status, providers & models, and token budgets.',
  apps: 'The App Library catalog: available, installed, and active apps with their RAM labels and requested permissions.',
  backup:
    'Backup targets and providers, schedules and retention, run history, and scheduled restore-test results.',
  tv: 'TV Mode: the 10-foot surface for the living room — media, photos, cameras, and household status on the big screen.',
  settings:
    'One door for everything: network, power, notifications, updates, users & security, and about.',
};

export function PlaceholderPage({ page }: { page: Exclude<PageId, 'home'> }) {
  return (
    <div className="placeholder">
      <p className="placeholder-text">{DESCRIPTIONS[page]}</p>
      <p className="placeholder-note">This section is a scaffold placeholder.</p>
    </div>
  );
}
