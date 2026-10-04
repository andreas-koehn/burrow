import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { ChevronLeft } from "lucide-react";
import { cx } from "./cx";

export interface PageHeaderProps {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  /** Parent page for sub-pages; renders "‹ <label>" above the title. */
  back?: { to: string; label: string };
  className?: string;
}

export function PageHeader({ title, subtitle, actions, back, className }: PageHeaderProps) {
  return (
    <div className={cx("page-header", className)}>
      <div className="left">
        {back && (
          <Link to={back.to} className="page-back" aria-label={`Back to ${back.label}`}>
            <ChevronLeft size={14} aria-hidden="true" />
            <span>{back.label}</span>
          </Link>
        )}
        <h1>{title}</h1>
        {subtitle != null && <p>{subtitle}</p>}
      </div>
      {actions != null && <div className="actions">{actions}</div>}
    </div>
  );
}
