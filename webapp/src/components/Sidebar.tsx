import { NAV_ITEMS, type PageId } from '../nav';
import { approvals, systemHealth } from '../mock';
import { Icon } from './Icon';

interface SidebarProps {
  current: PageId;
  onNavigate: (page: PageId) => void;
}

/** Brand mark: three bars in the tier colors — temperature at a glance. */
function BrandMark() {
  return (
    <svg width="30" height="30" viewBox="0 0 30 30" aria-hidden="true">
      <rect x="1" y="1" width="28" height="28" rx="8" fill="var(--surface-subtle)" />
      <rect x="7" y="8" width="16" height="3.4" rx="1.7" fill="var(--tier-hot)" />
      <rect x="7" y="13.3" width="16" height="3.4" rx="1.7" fill="var(--tier-warm)" />
      <rect x="7" y="18.6" width="16" height="3.4" rx="1.7" fill="var(--tier-cold)" />
    </svg>
  );
}

export function Sidebar({ current, onNavigate }: SidebarProps) {
  return (
    <aside className="sidebar">
      <div className="sidebar-brand">
        <BrandMark />
        <div className="sidebar-brand-text">
          <span className="sidebar-brand-name">KelviOS</span>
          <span className="sidebar-brand-status">
            <span className="dot dot--ok" aria-hidden="true" />
            {systemHealth.statusLabel}
          </span>
        </div>
      </div>

      <nav className="sidebar-nav" aria-label="Primary">
        {NAV_ITEMS.map((item) => (
          <button
            key={item.id}
            type="button"
            className={`nav-item${current === item.id ? ' nav-item--active' : ''}`}
            aria-current={current === item.id ? 'page' : undefined}
            onClick={() => onNavigate(item.id)}
          >
            <Icon name={item.icon} size={20} />
            <span className="nav-item-label">{item.label}</span>
            {item.id === 'approvals' && approvals.count > 0 && (
              <span className="badge badge--accent">{approvals.count}</span>
            )}
          </button>
        ))}
      </nav>

      <div className="sidebar-footer">
        <span className="sidebar-profile-avatar" aria-hidden="true">
          A
        </span>
        <div className="sidebar-profile-text">
          <span className="sidebar-profile-name">Admin</span>
          <span className="sidebar-profile-role">Owner</span>
        </div>
      </div>
    </aside>
  );
}
