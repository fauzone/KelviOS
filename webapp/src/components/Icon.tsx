/*
 * Inline SVG icons (stroke-based, 24×24). No icon library is used anywhere
 * in the UI — this component is the single source of glyphs.
 */

export type IconName =
  | 'home'
  | 'database'
  | 'folder'
  | 'activity'
  | 'approval'
  | 'cpu'
  | 'grid'
  | 'shield'
  | 'tv'
  | 'sliders'
  | 'search'
  | 'bell'
  | 'more'
  | 'logout';

const PATHS: Record<IconName, React.ReactNode> = {
  home: (
    <>
      <path d="M3 11.5 12 4l9 7.5" />
      <path d="M5.5 9.8V20h13V9.8" />
      <path d="M9.5 20v-5.5h5V20" />
    </>
  ),
  database: (
    <>
      <ellipse cx="12" cy="5.5" rx="7.5" ry="2.8" />
      <path d="M4.5 5.5v13c0 1.6 3.4 2.8 7.5 2.8s7.5-1.2 7.5-2.8v-13" />
      <path d="M4.5 12c0 1.6 3.4 2.8 7.5 2.8s7.5-1.2 7.5-2.8" />
    </>
  ),
  folder: (
    <path d="M3 7.5A1.5 1.5 0 0 1 4.5 6h5.2l2 2.5H19.5A1.5 1.5 0 0 1 21 10v8a1.5 1.5 0 0 1-1.5 1.5h-15A1.5 1.5 0 0 1 3 18Z" />
  ),
  activity: <path d="M3 12h4l2.5-6.5 4 13 2.5-6.5H21" />,
  approval: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="m8.5 12.4 2.4 2.4 4.6-5.3" />
    </>
  ),
  cpu: (
    <>
      <rect x="7" y="7" width="10" height="10" rx="2" />
      <rect x="10.4" y="10.4" width="3.2" height="3.2" rx="0.8" />
      <path d="M10 7V4.5M14 7V4.5M10 19.5V17M14 19.5V17M7 10H4.5M7 14H4.5M19.5 10H17M19.5 14H17" />
    </>
  ),
  grid: (
    <>
      <rect x="4.5" y="4.5" width="6.5" height="6.5" rx="1.6" />
      <rect x="13" y="4.5" width="6.5" height="6.5" rx="1.6" />
      <rect x="4.5" y="13" width="6.5" height="6.5" rx="1.6" />
      <rect x="13" y="13" width="6.5" height="6.5" rx="1.6" />
    </>
  ),
  shield: (
    <>
      <path d="M12 3l7 2.5V11c0 4.6-3 8.5-7 10-4-1.5-7-5.4-7-10V5.5Z" />
      <path d="m9.2 11.6 2 2 3.8-4.4" />
    </>
  ),
  tv: (
    <>
      <rect x="3" y="5" width="18" height="12.5" rx="2" />
      <path d="M9.5 21h5M12 17.5V21" />
    </>
  ),
  sliders: (
    <>
      <path d="M4 7h8.5M18.5 7H20M4 17h2.5M12.5 17H20" />
      <circle cx="15.5" cy="7" r="2.1" />
      <circle cx="9.5" cy="17" r="2.1" />
    </>
  ),
  search: (
    <>
      <circle cx="11" cy="11" r="6.5" />
      <path d="m15.8 15.8 4.7 4.7" />
    </>
  ),
  bell: (
    <>
      <path d="M6 16v-4.8a6 6 0 1 1 12 0V16l1.3 2.9a.7.7 0 0 1-.6 1H5.3a.7.7 0 0 1-.6-1Z" />
      <path d="M10.2 20.5a2 2 0 0 0 3.6 0" />
    </>
  ),
  more: (
    <>
      <circle cx="5" cy="12" r="1.4" fill="currentColor" stroke="none" />
      <circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none" />
      <circle cx="19" cy="12" r="1.4" fill="currentColor" stroke="none" />
    </>
  ),
  logout: (
    <>
      <path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4" />
      <path d="M10 17l-5-5 5-5" />
      <path d="M15 12H3" />
    </>
  ),
};

interface IconProps {
  name: IconName;
  size?: number;
  className?: string;
}

export function Icon({ name, size = 20, className }: IconProps) {
  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {PATHS[name]}
    </svg>
  );
}
