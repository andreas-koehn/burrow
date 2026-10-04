import type { ReactNode } from "react";
import { Inbox, AlertTriangle, ShieldAlert, Info, CircleCheck } from "lucide-react";
import { cx } from "./cx";

/* ── EmptyState ──────────────────────────────────────────────────
   Centered .state-card: thin-stroke icon bubble, one-sentence body,
   single primary action. Copy is supplied by the caller (C2). */
export interface EmptyStateProps {
  title: ReactNode;
  children?: ReactNode;
  icon?: ReactNode;
  action?: ReactNode;
  className?: string;
}

export function EmptyState({ title, children, icon, action, className }: EmptyStateProps) {
  return (
    <div className={cx("state-card", className)}>
      <div className="icon-bubble">{icon ?? <Inbox size={18} />}</div>
      <h4>{title}</h4>
      {children != null && <p>{children}</p>}
      {action}
    </div>
  );
}

/* ── ErrorNotice ─────────────────────────────────────────────────
   Left-bordered inline notice — never a flooded red background.
   role defaults to "alert" so it stays the page error region (C4). */
export interface ErrorNoticeProps {
  children: ReactNode;
  title?: ReactNode;
  detail?: ReactNode;
  action?: ReactNode;
  variant?: "error" | "warn" | "info" | "success";
  role?: string;
  className?: string;
}

const NOTICE_ICON = {
  error: AlertTriangle,
  warn: AlertTriangle,
  info: Info,
  success: CircleCheck,
} as const;

export function ErrorNotice({
  children,
  title,
  detail,
  action,
  variant = "error",
  role = "alert",
  className,
}: ErrorNoticeProps) {
  const Icon = NOTICE_ICON[variant];
  return (
    <div
      className={cx("notice-inline", variant !== "error" && variant, className)}
      role={role}
      data-variant={variant}
    >
      <span className="icon">
        <Icon size={14} />
      </span>
      <div className="body">
        {title && <strong>{title} </strong>}
        {children}
        {detail != null && <span className="mono">{detail}</span>}
      </div>
      {action && <span className="actions">{action}</span>}
    </div>
  );
}

/* ── SkeletonRows ────────────────────────────────────────────────
   n shimmer rows that mirror a table/list while loading. */
export interface SkeletonRowsProps {
  n?: number;
  className?: string;
}

export function SkeletonRows({ n = 4, className }: SkeletonRowsProps) {
  return (
    <div className={cx("col", "gap-3", className)} aria-hidden="true">
      {Array.from({ length: n }).map((_, i) => (
        <div key={i} className="row gap-3" style={{ alignItems: "center" }}>
          <div className="skel" style={{ width: 28, height: 28, borderRadius: "50%" }} />
          <div className="col gap-2" style={{ flex: 1 }}>
            <div className="skel" style={{ width: "40%", height: 11 }} />
            <div className="skel" style={{ width: "24%", height: 9 }} />
          </div>
          <div className="skel" style={{ width: 56, height: 14, borderRadius: 4 }} />
          <div className="skel" style={{ width: 70, height: 10 }} />
        </div>
      ))}
    </div>
  );
}

/* ── NotAuthorized ───────────────────────────────────────────────
   Calm centered notice for 403 — same .state-card shape as Empty,
   with a warning bubble. Copy supplied by the caller (C2). */
export interface NotAuthorizedProps {
  title: ReactNode;
  children?: ReactNode;
  detail?: ReactNode;
  icon?: ReactNode;
  action?: ReactNode;
  className?: string;
}

export function NotAuthorized({
  title,
  children,
  detail,
  icon,
  action,
  className,
}: NotAuthorizedProps) {
  return (
    <div className={cx("state-card", className)}>
      <div className="icon-bubble">{icon ?? <ShieldAlert size={18} />}</div>
      <h4>{title}</h4>
      {children != null && <p>{children}</p>}
      {detail != null && <span className="mono">{detail}</span>}
      {action}
    </div>
  );
}
