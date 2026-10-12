import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { cx } from "./cx";

export interface MetricStripProps {
  ariaLabel: string;
  children: ReactNode;
  className?: string;
}

export function MetricStrip({ ariaLabel, children, className }: MetricStripProps) {
  return (
    <div role="list" aria-label={ariaLabel} className={cx("metric-strip", className)}>
      {children}
    </div>
  );
}

export interface MetricTileProps {
  label: ReactNode;
  value: ReactNode;
  sub?: ReactNode;
  tooltip?: string;
  /** Where the figure is broken down; makes label, value and sub one link. */
  to?: string;
  /**
   * Makes label, value and sub one toggle button: the tile is one of a set of
   * choices and `pressed` says whether it is the chosen one. Not with `to`.
   */
  onPress?: () => void;
  pressed?: boolean;
  children?: ReactNode;
  className?: string;
}

export function MetricTile({ label, value, sub, tooltip, to, onPress, pressed = false, children, className }: MetricTileProps) {
  const figure = (
    <>
      <span className="label">{label}</span>
      <span className="value">{value}</span>
      {sub != null && <span className="sub">{sub}</span>}
    </>
  );
  return (
    <div role="listitem" title={tooltip} className={cx("metric-tile", (to || onPress) && "is-link", onPress && pressed && "is-selected", className)}>
      {to ? <Link to={to} className="metric-tile-link">{figure}</Link>
        : onPress ? <button type="button" className="metric-tile-link" aria-pressed={pressed} onClick={onPress}>{figure}</button>
        : figure}
      {/* Outside the link or button, so a control among them never nests inside it. */}
      {children}
    </div>
  );
}
