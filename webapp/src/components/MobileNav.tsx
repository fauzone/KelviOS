import { useState } from 'react';
import { MOBILE_ANCHORS, MORE_ITEMS, type PageId } from '../nav';
import { approvals } from '../mock';
import { Icon } from './Icon';

interface MobileNavProps {
  current: PageId;
  onNavigate: (page: PageId) => void;
}

/**
 * Mobile bottom navigation: exactly five anchors — Home · Storage ·
 * Activity · Agents · More (brief §2). "More" opens a sheet with the
 * remaining sections. All tap targets are ≥48px (styles.css).
 */
export function MobileNav({ current, onNavigate }: MobileNavProps) {
  const [moreOpen, setMoreOpen] = useState(false);
  const moreIsActive = MORE_ITEMS.some((item) => item.id === current);

  const go = (page: PageId) => {
    setMoreOpen(false);
    onNavigate(page);
  };

  return (
    <>
      {moreOpen && (
        <div
          className="more-overlay"
          role="presentation"
          onClick={() => setMoreOpen(false)}
        >
          <div
            className="more-sheet"
            role="menu"
            aria-label="More sections"
            onClick={(event) => event.stopPropagation()}
          >
            {MORE_ITEMS.map((item) => (
              <button
                key={item.id}
                type="button"
                role="menuitem"
                className={`more-sheet-item${current === item.id ? ' more-sheet-item--active' : ''}`}
                onClick={() => go(item.id)}
              >
                <Icon name={item.icon} size={21} />
                <span>{item.label}</span>
                {item.id === 'approvals' && approvals.count > 0 && (
                  <span className="badge badge--accent">{approvals.count}</span>
                )}
              </button>
            ))}
          </div>
        </div>
      )}

      <nav className="mobile-nav" aria-label="Mobile">
        {MOBILE_ANCHORS.map((item) => (
          <button
            key={item.id}
            type="button"
            className={`mobile-nav-item${current === item.id ? ' mobile-nav-item--active' : ''}`}
            aria-current={current === item.id ? 'page' : undefined}
            onClick={() => go(item.id)}
          >
            <Icon name={item.icon} size={22} />
            <span>{item.label}</span>
          </button>
        ))}
        <button
          type="button"
          className={`mobile-nav-item${moreIsActive || moreOpen ? ' mobile-nav-item--active' : ''}`}
          aria-expanded={moreOpen}
          onClick={() => setMoreOpen((open) => !open)}
        >
          <Icon name="more" size={22} />
          <span>More</span>
          {approvals.count > 0 && <span className="mobile-nav-dot" aria-hidden="true" />}
        </button>
      </nav>
    </>
  );
}
