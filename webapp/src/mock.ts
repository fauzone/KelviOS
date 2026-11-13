/*
 * Mock data for the KelviOS UI scaffold.
 *
 * Everything the shell and Home page render comes from this module so the
 * real API layer can replace it in one place later. No component fetches
 * anything; there are no backend calls in this workstream.
 */

export type StateKind = 'ok' | 'warning' | 'danger' | 'neutral';
export type TierId = 'hot' | 'warm' | 'cold';
export type PowerState = 'Awake' | 'Idle' | 'Asleep';

export interface TierCapacity {
  id: TierId;
  label: string;
  usedGB: number;
  totalGB: number;
  power: PowerState;
  mechanism: string;
}

export const systemHealth = {
  state: 'ok' as StateKind,
  statusLabel: 'Healthy',
  disksOk: 6,
  disksTotal: 6,
  uptime: '12 days, 4 hrs',
  detail: 'All disks reporting normal SMART status.',
};

export const tiers: TierCapacity[] = [
  { id: 'hot', label: 'Hot', usedGB: 412, totalGB: 1024, power: 'Awake', mechanism: 'SSD · Mirror' },
  { id: 'warm', label: 'Warm', usedGB: 5734, totalGB: 8192, power: 'Idle', mechanism: 'Pool + Parity' },
  { id: 'cold', label: 'Cold', usedGB: 3901, totalGB: 7168, power: 'Asleep', mechanism: 'Pool + Parity' },
];

export const backupStatus = {
  state: 'ok' as StateKind,
  statusLabel: 'Protected',
  lastRun: 'Today, 03:00',
  lastResult: 'Success',
  target: 'Google Drive',
  nextRun: 'Tomorrow, 03:00',
};

export interface ActivityEvent {
  id: number;
  time: string;
  text: string;
  kind: 'backup' | 'lifecycle' | 'system' | 'agent';
}

export const recentActivity: ActivityEvent[] = [
  { id: 1, time: '09:41', text: 'Backup to Google Drive completed — 2.1 GB delta', kind: 'backup' },
  { id: 2, time: '08:15', text: 'Cold pool spun down after 60 minutes idle', kind: 'system' },
  { id: 3, time: '07:52', text: 'Tier Curator prepared 1 lifecycle recommendation', kind: 'agent' },
  { id: 4, time: '03:00', text: 'Nightly backup started (Warm + Hot)', kind: 'backup' },
];

export interface AgentStatus {
  name: string;
  focus: string;
  status: string;
  state: StateKind;
}

export const agents: AgentStatus[] = [
  { name: 'Health Guardian', focus: 'Disk health & SMART', status: 'Active', state: 'ok' },
  { name: 'Backup Steward', focus: 'Backup schedules & restore tests', status: 'Active', state: 'ok' },
  { name: 'Tier Curator', focus: 'Tier placement & shadows', status: '1 recommendation', state: 'warning' },
  { name: 'Capacity Planner', focus: 'Growth & disk planning', status: 'Active', state: 'ok' },
];

export const approvals = {
  count: 3,
  topItem: {
    title: 'Move “Photos 2019–2022” to Cold',
    detail: 'Warm → Cold · 412 GB · not accessed in 64 days',
    risk: 'Low risk',
  },
};

export const notifications = {
  count: 2,
};

/** Format a GB figure as GB / TB with tabular-friendly precision. */
export function formatCapacity(gb: number): string {
  if (gb >= 1024) {
    const tb = gb / 1024;
    return `${tb >= 10 ? tb.toFixed(1) : tb.toFixed(2)} TB`;
  }
  return `${Math.round(gb)} GB`;
}

/** Density mode for the Home dashboard. */
export type DensityMode = 'simple' | 'moderate' | 'detailed';

/** Default density mode. */
export const DEFAULT_DENSITY: DensityMode = 'simple';

/** localStorage key for density preference. */
export const DENSITY_STORAGE_KEY = 'kelvios-density';

/** Get saved density mode from localStorage. */
export function getSavedDensity(): DensityMode {
  if (typeof window === 'undefined') return DEFAULT_DENSITY;
  const saved = localStorage.getItem(DENSITY_STORAGE_KEY);
  if (saved === 'simple' || saved === 'moderate' || saved === 'detailed') {
    return saved;
  }
  return DEFAULT_DENSITY;
}

/** Save density mode to localStorage. */
export function saveDensity(mode: DensityMode): void {
  if (typeof window !== 'undefined') {
    localStorage.setItem(DENSITY_STORAGE_KEY, mode);
  }
}

// ===== MOCK DATA FOR MODERATE & DETAILED MODES =====
// All new data is MOCK — real API layer will replace later.

/** Per-pool condition (for Moderate mode). */
export interface PoolCondition {
  id: string;
  name: string;
  state: StateKind;
  disksTotal: number;
  disksHealthy: number;
  capacityUsedGB: number;
  capacityTotalGB: number;
}

export const poolConditions: PoolCondition[] = [
  { id: 'pool-hot', name: 'Hot Pool', state: 'ok', disksTotal: 2, disksHealthy: 2, capacityUsedGB: 412, capacityTotalGB: 1024 },
  { id: 'pool-warm', name: 'Warm Pool', state: 'warning', disksTotal: 4, disksHealthy: 3, capacityUsedGB: 5734, capacityTotalGB: 8192 },
  { id: 'pool-cold', name: 'Cold Pool', state: 'ok', disksTotal: 4, disksHealthy: 4, capacityUsedGB: 3901, capacityTotalGB: 7168 },
];

/** Per-disk condition (for Moderate mode). */
export interface DiskCondition {
  id: string;
  poolId: string;
  name: string;
  state: StateKind;
  model: string;
  serial: string;
  capacityGB: number;
  temperatureC: number;
  smartStatus: 'pass' | 'fail' | 'unknown';
}

export const diskConditions: DiskCondition[] = [
  { id: 'disk-1', poolId: 'pool-hot', name: 'nvme0n1', state: 'ok', model: 'Samsung 980 Pro 1TB', serial: 'S12345', capacityGB: 1024, temperatureC: 38, smartStatus: 'pass' },
  { id: 'disk-2', poolId: 'pool-hot', name: 'nvme1n1', state: 'ok', model: 'Samsung 980 Pro 1TB', serial: 'S12346', capacityGB: 1024, temperatureC: 41, smartStatus: 'pass' },
  { id: 'disk-3', poolId: 'pool-warm', name: 'sda', state: 'ok', model: 'Seagate IronWolf 8TB', serial: 'W78901', capacityGB: 8192, temperatureC: 32, smartStatus: 'pass' },
  { id: 'disk-4', poolId: 'pool-warm', name: 'sdb', state: 'warning', model: 'Seagate IronWolf 8TB', serial: 'W78902', capacityGB: 8192, temperatureC: 45, smartStatus: 'pass' },
  { id: 'disk-5', poolId: 'pool-warm', name: 'sdc', state: 'ok', model: 'Seagate IronWolf 8TB', serial: 'W78903', capacityGB: 8192, temperatureC: 34, smartStatus: 'pass' },
  { id: 'disk-6', poolId: 'pool-warm', name: 'sdd', state: 'ok', model: 'Seagate IronWolf 8TB', serial: 'W78904', capacityGB: 8192, temperatureC: 31, smartStatus: 'pass' },
  { id: 'disk-7', poolId: 'pool-cold', name: 'sde', state: 'ok', model: 'WD Red Plus 8TB', serial: 'C56781', capacityGB: 8192, temperatureC: 28, smartStatus: 'pass' },
  { id: 'disk-8', poolId: 'pool-cold', name: 'sdf', state: 'ok', model: 'WD Red Plus 8TB', serial: 'C56782', capacityGB: 8192, temperatureC: 27, smartStatus: 'pass' },
  { id: 'disk-9', poolId: 'pool-cold', name: 'sdg', state: 'ok', model: 'WD Red Plus 8TB', serial: 'C56783', capacityGB: 8192, temperatureC: 29, smartStatus: 'pass' },
  { id: 'disk-10', poolId: 'pool-cold', name: 'sdh', state: 'ok', model: 'WD Red Plus 8TB', serial: 'C56784', capacityGB: 8192, temperatureC: 28, smartStatus: 'pass' },
];

/** Network interface status (for Moderate mode; folded into top strip for Detailed). */
export interface NetworkInterface {
  name: string;
  state: StateKind;
  speed: string;
  rxMbps: number;
  txMbps: number;
}

export const networkInterfaces: NetworkInterface[] = [
  { name: 'eth0', state: 'ok', speed: '10 Gbps', rxMbps: 1240, txMbps: 890 },
  { name: 'eth1', state: 'ok', speed: '1 Gbps', rxMbps: 45, txMbps: 12 },
];

/** Throughput per-pool (for Detailed mode). */
export interface PoolThroughput {
  poolId: string;
  readMBps: number;
  writeMBps: number;
  iopsRead: number;
  iopsWrite: number;
}

export const poolThroughputs: PoolThroughput[] = [
  { poolId: 'pool-hot', readMBps: 2840, writeMBps: 1920, iopsRead: 45200, iopsWrite: 31800 },
  { poolId: 'pool-warm', readMBps: 1120, writeMBps: 890, iopsRead: 18400, iopsWrite: 14200 },
  { poolId: 'pool-cold', readMBps: 45, writeMBps: 12, iopsRead: 820, iopsWrite: 310 },
];

/** Cron job status (for Detailed mode). */
export interface CronJobStatus {
  id: string;
  name: string;
  schedule: string;
  lastRun: string;
  lastResult: 'success' | 'failed' | 'running' | 'skipped';
  nextRun: string;
}

export const cronJobs: CronJobStatus[] = [
  { id: 'cron-backup-nightly', name: 'Nightly Backup', schedule: 'Daily 03:00', lastRun: 'Today 03:00', lastResult: 'success', nextRun: 'Tomorrow 03:00' },
  { id: 'cron-scrub-weekly', name: 'Weekly Scrub', schedule: 'Sun 02:00', lastRun: '3 days ago', lastResult: 'success', nextRun: 'In 4 days' },
  { id: 'cron-smart-daily', name: 'SMART Check', schedule: 'Daily 06:00', lastRun: 'Today 06:00', lastResult: 'success', nextRun: 'Tomorrow 06:00' },
  { id: 'cron-tier-curator', name: 'Tier Curator', schedule: 'Every 6h', lastRun: '2 hours ago', lastResult: 'success', nextRun: 'In 4 hours' },
  { id: 'cron-capacity-plan', name: 'Capacity Planner', schedule: 'Weekly Mon 09:00', lastRun: '5 days ago', lastResult: 'success', nextRun: 'In 2 days' },
];

/** Automation rule status (for Detailed mode). */
export interface AutomationRuleStatus {
  id: string;
  name: string;
  trigger: string;
  action: string;
  state: 'enabled' | 'disabled' | 'error';
  lastTriggered: string | null;
}

export const automationRules: AutomationRuleStatus[] = [
  { id: 'auto-spin-down-cold', name: 'Spin Down Cold Pool', trigger: 'Idle 60 min', action: 'Set pool power=Asleep', state: 'enabled', lastTriggered: 'Today 08:15' },
  { id: 'auto-wake-on-access', name: 'Wake on Access', trigger: 'Cold pool read request', action: 'Set pool power=Awake', state: 'enabled', lastTriggered: 'Today 09:41' },
  { id: 'auto-tier-recommend', name: 'Tier Recommendation', trigger: 'Weekly analysis', action: 'Create move proposal', state: 'enabled', lastTriggered: '3 days ago' },
  { id: 'auto-alert-disk-temp', name: 'Disk Temp Alert', trigger: 'Disk > 50°C', action: 'Notify + throttle', state: 'enabled', lastTriggered: null },
  { id: 'auto-backup-verify', name: 'Backup Verify', trigger: 'Post-backup', action: 'Test restore sample', state: 'disabled', lastTriggered: null },
];

/** System vitals (for Detailed mode). */
export interface SystemVitals {
  cpuPercent: number;
  memoryPercent: number;
  memoryUsedGB: number;
  memoryTotalGB: number;
  loadAvg1m: number;
  loadAvg5m: number;
  loadAvg15m: number;
  uptime: string;
}

export const systemVitals: SystemVitals = {
  cpuPercent: 12,
  memoryPercent: 38,
  memoryUsedGB: 12.4,
  memoryTotalGB: 32,
  loadAvg1m: 0.84,
  loadAvg5m: 0.72,
  loadAvg15m: 0.68,
  uptime: '12 days, 4 hrs',
};
