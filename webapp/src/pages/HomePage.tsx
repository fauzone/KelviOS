import { useState, useEffect, type ReactNode } from 'react';
import {
  agents,
  approvals,
  automationRules,
  backupStatus,
  cronJobs,
  diskConditions,
  formatCapacity,
  networkInterfaces,
  poolConditions,
  poolThroughputs,
  recentActivity,
  systemHealth,
  systemVitals,
  tiers,
  type DensityMode,
  getSavedDensity,
  saveDensity,
} from '../mock';
import type { PageId } from '../nav';

function Card({
  label,
  children,
  className = '',
}: {
  label: string;
  children: ReactNode;
  className?: string;
}) {
  return (
    <section className={`card ${className}`}>
      <h2 className="card-label">{label}</h2>
      {children}
    </section>
  );
}

function DensitySwitcher({
  value,
  onChange,
}: {
  value: DensityMode;
  onChange: (mode: DensityMode) => void;
}) {
  const modes: { id: DensityMode; label: string }[] = [
    { id: 'simple', label: 'Simple' },
    { id: 'moderate', label: 'Moderate' },
    { id: 'detailed', label: 'Detailed' },
  ];

  return (
    <div className="density-switcher" role="group" aria-label="Dashboard density">
      {modes.map((mode) => (
        <button
          key={mode.id}
          type="button"
          className={`density-switcher-btn ${value === mode.id ? 'density-switcher-btn--active' : ''}`}
          onClick={() => onChange(mode.id)}
          aria-pressed={value === mode.id}
        >
          {mode.label}
        </button>
      ))}
    </div>
  );
}

function ApprovalStrip({ onNavigate }: { onNavigate: (page: PageId) => void }) {
  if (approvals.count === 0) return null;
  return (
    <div className="approval-strip">
      <div className="approval-strip-text">
        <p className="approval-strip-title">
          {approvals.count} approvals waiting
        </p>
        <p className="approval-strip-detail">
          {approvals.topItem.title} — {approvals.topItem.detail} · {' '}
          {approvals.topItem.risk}
        </p>
      </div>
      <button
        type="button"
        className="button button--primary"
        onClick={() => onNavigate('approvals')}
      >
        Review
      </button>
    </div>
  );
}

function SystemHealthCard() {
  return (
    <Card label="System health">
      <p className="kpi-number num">{systemHealth.statusLabel}</p>
      <p className="card-sub">
        <span className="dot dot--ok" aria-hidden="true" />
        {systemHealth.disksOk} of {systemHealth.disksTotal} disks OK
      </p>
      <p className="card-sub">Uptime {systemHealth.uptime}</p>
    </Card>
  );
}

function CapacityCard() {
  const totalUsed = tiers.reduce((sum, tier) => sum + tier.usedGB, 0);
  const totalCapacity = tiers.reduce((sum, tier) => sum + tier.totalGB, 0);

  return (
    <Card label="Capacity by tier">
      <p className="kpi-number num">
        {formatCapacity(totalUsed)}
        <span className="kpi-number-unit"> of {formatCapacity(totalCapacity)}</span>
      </p>
      <div className="tier-bar" aria-hidden="true">
        {tiers.map((tier) => (
          <span
            key={tier.id}
            className={`tier-bar-segment tier-bar-segment--${tier.id}`}
            style={{ width: `${(tier.usedGB / totalCapacity) * 100}%` }}
          />
        ))}
      </div>
      <ul className="tier-legend">
        {tiers.map((tier) => (
          <li key={tier.id} className="tier-legend-row">
            <span className={`dot dot--tier-${tier.id}`} aria-hidden="true" />
            <span className="tier-legend-name">{tier.label}</span>
            <span className="tier-legend-figures num">
              {formatCapacity(tier.usedGB)} / {formatCapacity(tier.totalGB)}
            </span>
            <span className={`power-badge power-badge--${tier.power.toLowerCase()}`}>
              {tier.power}
            </span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function ProtectionCard() {
  return (
    <Card label="Protection">
      <p className="kpi-number num">{backupStatus.statusLabel}</p>
      <p className="card-sub">
        Last backup {backupStatus.lastRun} · {backupStatus.lastResult}
      </p>
      <p className="card-sub">
        Target {backupStatus.target} · Next {backupStatus.nextRun}
      </p>
    </Card>
  );
}

function AgentsCard() {
  return (
    <Card label="Agents">
      <ul className="agent-list">
        {agents.map((agent) => (
          <li key={agent.name} className="agent-row">
            <span className={`dot dot--${agent.state}`} aria-hidden="true" />
            <span className="agent-name">{agent.name}</span>
            <span className="agent-status">{agent.status}</span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function RecentActivityCard() {
  return (
    <Card label="Recent activity" className="card--wide">
      <ul className="activity-list">
        {recentActivity.map((event) => (
          <li key={event.id} className="activity-row">
            <span className="activity-time num">{event.time}</span>
            <span className="activity-text">{event.text}</span>
          </li>
        ))}
      </ul>
    </Card>
  );
}

function QuickActions({ onNavigate }: { onNavigate: (page: PageId) => void }) {
  return (
    <div className="quick-actions">
      <button type="button" className="button" onClick={() => onNavigate('storage')}>Wake Cold</button>
      <button type="button" className="button" onClick={() => onNavigate('backup')}>Run Backup Now</button>
      <button type="button" className="button" onClick={() => onNavigate('storage')}>Create Pool</button>
    </div>
  );
}

// ===== MODERATE MODE COMPONENTS =====

function PoolConditionsCard() {
  return (
    <Card label="Pool conditions">
      <div className="pool-conditions">
        {poolConditions.map((pool) => (
          <div key={pool.id} className="pool-condition-row">
            <div className="pool-condition-main">
              <span className={`dot dot--${pool.state}`} aria-hidden="true" />
              <span className="pool-condition-name">{pool.name}</span>
              <span className="pool-condition-disks num">
                {pool.disksHealthy}/{pool.disksTotal} disks OK
              </span>
            </div>
            <div className="pool-condition-capacity">
              {formatCapacity(pool.capacityUsedGB)} / {formatCapacity(pool.capacityTotalGB)}
            </div>
          </div>
        ))}
      </div>
    </Card>
  );
}

function DiskConditionsCard() {
  return (
    <Card label="Disk conditions">
      <div className="disk-conditions">
        {diskConditions.map((disk) => (
          <div key={disk.id} className="disk-condition-row">
            <span className={`dot dot--${disk.state}`} aria-hidden="true" />
            <span className="disk-condition-name">{disk.name}</span>
            <span className="disk-condition-model">{disk.model}</span>
            <span className="disk-condition-capacity num">{formatCapacity(disk.capacityGB)}</span>
            <span className="disk-condition-temp num">{disk.temperatureC}°C</span>
            <span className={`disk-condition-smart disk-condition-smart--${disk.smartStatus}`}>
              {disk.smartStatus === 'pass' ? 'SMART OK' : disk.smartStatus === 'fail' ? 'SMART FAIL' : 'Unknown'}
            </span>
          </div>
        ))}
      </div>
    </Card>
  );
}

function NetworkStatusStrip() {
  return (
    <div className="network-status-strip">
      {networkInterfaces.map((iface) => (
        <div key={iface.name} className="network-iface">
          <span className="network-iface-name">{iface.name}</span>
          <span className={`dot dot--${iface.state}`} aria-hidden="true" />
          <span className="network-iface-speed">{iface.speed}</span>
          <span className="network-iface-throughput">
            ↓ {iface.rxMbps} Mbps ↑ {iface.txMbps} Mbps
          </span>
        </div>
      ))}
    </div>
  );
}

// ===== DETAILED MODE COMPONENTS =====

function PoolThroughputCard() {
  return (
    <Card label="Pool throughput">
      <div className="pool-throughput">
        {poolThroughputs.map((tp) => (
          <div key={tp.poolId} className="pool-throughput-row">
            <span className="pool-throughput-pool">{tp.poolId.replace('pool-', '').toUpperCase()}</span>
            <div className="pool-throughput-metrics">
              <span className="pool-throughput-metric">
                <span className="pool-throughput-label">Read</span>
                <span className="pool-throughput-value num">{tp.readMBps} MB/s</span>
              </span>
              <span className="pool-throughput-metric">
                <span className="pool-throughput-label">Write</span>
                <span className="pool-throughput-value num">{tp.writeMBps} MB/s</span>
              </span>
              <span className="pool-throughput-metric">
                <span className="pool-throughput-label">IOPS R/W</span>
                <span className="pool-throughput-value num">{tp.iopsRead.toLocaleString()} / {tp.iopsWrite.toLocaleString()}</span>
              </span>
            </div>
          </div>
        ))}
      </div>
    </Card>
  );
}

function CronAutomationCard() {
  return (
    <Card label="Cron & Automation">
      <div className="cron-automation">
        <div className="cron-section">
          <h3 className="cron-automation-subtitle">Scheduled Jobs</h3>
          <div className="cron-list">
            {cronJobs.map((job) => (
              <div key={job.id} className="cron-row">
                <div className="cron-info">
                  <span className="cron-name">{job.name}</span>
                  <span className="cron-schedule">{job.schedule}</span>
                </div>
                <div className="cron-status">
                  <span className={`cron-badge cron-badge--${job.lastResult}`}>{job.lastResult}</span>
                  <span className="cron-times">Last: {job.lastRun} · Next: {job.nextRun}</span>
                </div>
              </div>
            ))}
          </div>
        </div>
        <div className="automation-section">
          <h3 className="cron-automation-subtitle">Automation Rules</h3>
          <div className="automation-list">
            {automationRules.map((rule) => (
              <div key={rule.id} className="automation-row">
                <div className="automation-info">
                  <span className="automation-name">{rule.name}</span>
                  <span className="automation-trigger">{rule.trigger} → {rule.action}</span>
                </div>
                <div className="automation-status">
                  <span className={`automation-badge automation-badge--${rule.state}`}>{rule.state}</span>
                  {rule.lastTriggered && (
                    <span className="automation-last">Last: {rule.lastTriggered}</span>
                  )}
                </div>
              </div>
            ))}
          </div>
        </div>
      </div>
    </Card>
  );
}

function SystemVitalsCard() {
  return (
    <Card label="System vitals">
      <div className="system-vitals">
        <div className="vitals-grid">
          <div className="vital-item">
            <span className="vital-label">CPU</span>
            <span className="vital-value num">{systemVitals.cpuPercent}%</span>
          </div>
          <div className="vital-item">
            <span className="vital-label">Memory</span>
            <span className="vital-value num">{systemVitals.memoryPercent}%</span>
            <span className="vital-detail num">{formatCapacity(systemVitals.memoryUsedGB * 1024)} / {formatCapacity(systemVitals.memoryTotalGB * 1024)}</span>
          </div>
          <div className="vital-item">
            <span className="vital-label">Load avg</span>
            <span className="vital-value num">{systemVitals.loadAvg1m} / {systemVitals.loadAvg5m} / {systemVitals.loadAvg15m}</span>
          </div>
          <div className="vital-item">
            <span className="vital-label">Uptime</span>
            <span className="vital-value">{systemVitals.uptime}</span>
          </div>
        </div>
      </div>
    </Card>
  );
}

function NetworkStatusTopStrip() {
  return (
    <div className="network-status-top-strip">
      {networkInterfaces.map((iface) => (
        <div key={iface.name} className="network-top-iface">
          <span className="network-top-name">{iface.name}</span>
          <span className={`dot dot--${iface.state}`} aria-hidden="true" />
          <span className="network-top-speed">{iface.speed}</span>
          <span className="network-top-throughput">↓{iface.rxMbps} ↑{iface.txMbps} Mbps</span>
        </div>
      ))}
    </div>
  );
}

export function HomePage({ onNavigate }: { onNavigate: (page: PageId) => void }) {
  const [density, setDensity] = useState<DensityMode>(() => getSavedDensity());

  useEffect(() => {
    saveDensity(density);
  }, [density]);

  const isModerate = density === 'moderate' || density === 'detailed';
  const isDetailed = density === 'detailed';

  return (
    <div className="home">
      {/* Approval strip - INVARIANT: shows in ALL modes */}
      <ApprovalStrip onNavigate={onNavigate} />

      {/* Density switcher in header area */}
      <div className="home-header">
        <h1 className="home-title">Dashboard</h1>
        <DensitySwitcher value={density} onChange={setDensity} />
      </div>

      {/* Top stat cards - always present */}
      <div className="kpi-grid">
        <SystemHealthCard />
        <CapacityCard />
        <ProtectionCard />
        <AgentsCard />
      </div>

      {/* Network status: separate strip in Moderate, folded into top in Detailed */}
      {isModerate && !isDetailed && <NetworkStatusStrip />}
      {isDetailed && <NetworkStatusTopStrip />}

      {/* Moderate: Pool & Disk conditions */}
      {isModerate && !isDetailed && (
        <div className="kpi-grid">
          <PoolConditionsCard />
          <DiskConditionsCard />
        </div>
      )}

      {/* Detailed: Pools & Disks as vertical columns side-by-side with Cron & Automation */}
      {isDetailed && (
        <div className="detailed-main-grid">
          <div className="detailed-left-column">
            <PoolConditionsCard />
            <DiskConditionsCard />
            <PoolThroughputCard />
          </div>
          <div className="detailed-right-column">
            <CronAutomationCard />
            <SystemVitalsCard />
          </div>
        </div>
      )}

      {/* Recent activity - always present */}
      <RecentActivityCard />

      {/* Quick actions - always present */}
      <QuickActions onNavigate={onNavigate} />
    </div>
  );
}