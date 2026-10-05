import { Link } from "react-router-dom";
import { PanelLeftClose, PanelLeftOpen } from "lucide-react";

export interface TopBarProps {
  crumbs: { label: string; to?: string }[];
  collapsed: boolean;
  onToggle: () => void;
}

/** Thin bar above the page: the sidebar toggle and where you are. */
export function TopBar({ crumbs, collapsed, onToggle }: TopBarProps) {
  const label = collapsed ? "Expand sidebar" : "Collapse sidebar";
  return (
    <div className="topbar">
      <button type="button" className="icon-btn" aria-pressed={collapsed} aria-label={label} title={label} onClick={onToggle}>
        {collapsed ? <PanelLeftOpen size={15} aria-hidden="true" /> : <PanelLeftClose size={15} aria-hidden="true" />}
      </button>
      <nav aria-label="Breadcrumb">
        <ol className="crumbs">
          {crumbs.map((c, i) => (
            <li key={`${i}-${c.label}`}>
              {i > 0 && <span className="sep" aria-hidden="true">/</span>}
              {c.to
                ? <Link to={c.to}>{c.label}</Link>
                : <span aria-current={i === crumbs.length - 1 ? "page" : undefined}>{c.label}</span>}
            </li>
          ))}
        </ol>
      </nav>
    </div>
  );
}
