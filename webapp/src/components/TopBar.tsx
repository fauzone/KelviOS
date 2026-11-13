import { approvals, notifications } from '../mock';
import type { PageId } from '../nav';
import { Icon } from './Icon';

interface TopBarProps {
  title: string;
  onNavigate: (page: PageId) => void;
  onLogout: () => void;
}

export function TopBar({ title, onNavigate, onLogout }: TopBarProps) {
  return (
    <header className="topbar">
      <h1 className="topbar-title">{title}</h1>

      <div className="topbar-search">
        <Icon name="search" size={17} className="topbar-search-icon" />
        <input
          type="search"
          className="topbar-search-input"
          placeholder="Search files, settings, apps…"
          aria-label="Universal search"
        />
        <kbd className="topbar-search-kbd">⌘K</kbd>
      </div>

      <div className="topbar-actions">
        <button
          type="button"
          className="icon-button"
          aria-label={`Notifications, ${notifications.count} unread`}
        >
          <Icon name="bell" size={20} />
          {notifications.count > 0 && (
            <span className="badge badge--dot">{notifications.count}</span>
          )}
        </button>
        <button
          type="button"
          className="icon-button"
          aria-label={`Approvals, ${approvals.count} waiting`}
          onClick={() => onNavigate('approvals')}
        >
          <Icon name="approval" size={20} />
          {approvals.count > 0 && (
            <span className="badge badge--dot">{approvals.count}</span>
          )}
        </button>
        <button
          type="button"
          className="icon-button"
          aria-label="Log out"
          onClick={onLogout}
        >
          <Icon name="logout" size={20} />
        </button>
      </div>
    </header>
  );
}
