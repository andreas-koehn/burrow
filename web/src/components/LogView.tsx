import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ChevronRight } from "lucide-react";
import { ApiError } from "@/lib/api";
import { Button, ErrorNotice, Input, Segmented, SkeletonRows } from "@/components/ds";
import { TIME_RANGES, type TimeRange } from "@/lib/time-range";

export type { TimeRange } from "@/lib/time-range";

export interface LogColumn<Row> {
  id: string;
  header: string;
  cell: (row: Row) => ReactNode;
  numeric?: boolean;
}

export interface LogViewProps<Row> {
  /** Accessible name of the table, e.g. "Traffic". */
  label: string;
  rows: Row[] | undefined;
  rowKey: (row: Row) => string;
  columns: LogColumn<Row>[];
  isLoading: boolean;
  error: unknown;
  onRetry: () => void;
  /** Controlled filter state; LogView renders the controls, the page owns the query. */
  range: TimeRange;
  onRangeChange: (r: TimeRange) => void;
  search: string;
  onSearchChange: (s: string) => void;
  /** Extra controls, e.g. a service picker. */
  filters?: ReactNode;
  /** Shown when rows is an empty array. */
  empty: ReactNode;
  /** Row detail, shown in a panel beside the table. */
  detail?: (row: Row) => ReactNode;
  /** What tells a row from its neighbours, for the name of its detail button ("Show details for …"). */
  rowLabel?: (row: Row) => string;
  hasMore?: boolean;
  onLoadMore?: () => void;
}

/**
 * One log table for every scope: a filter bar (time range, search, the page's own
 * controls), then loading, error, empty or the rows, with an optional detail panel
 * beside them. The page owns the query and the filter state; nothing is fetched here.
 */
export function LogView<Row>({
  label, rows, rowKey, columns, isLoading, error, onRetry,
  range, onRangeChange, search, onSearchChange, filters, empty, detail, rowLabel, hasMore, onLoadMore,
}: LogViewProps<Row>) {
  const [openKey, setOpenKey] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const tableRef = useRef<HTMLTableElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const panelId = useId();
  const open = detail && openKey !== null ? rows?.find((r) => rowKey(r) === openKey) : undefined;
  // The open row left the list (a filter changed, a refresh dropped it): forget it,
  // so it does not spring open again when it comes back.
  if (openKey !== null && !open) setOpenKey(null);

  // The panel follows the table in the page; taking the focus there spares a keyboard
  // user the remaining rows and lets a screen reader announce what opened.
  useEffect(() => {
    if (openKey !== null) panelRef.current?.focus();
  }, [openKey]);

  // A panel that went away while it held the focus leaves the focus on <body>; keep it in the view.
  const hadPanel = useRef(false);
  useEffect(() => {
    if (hadPanel.current && !open && document.activeElement === document.body) rootRef.current?.focus();
    hadPanel.current = Boolean(open);
  }, [open]);

  const toggleOf = (key: string) =>
    [...(tableRef.current?.querySelectorAll<HTMLElement>("tbody tr") ?? [])]
      .find((tr) => tr.dataset.rowKey === key)
      ?.querySelector<HTMLElement>("button[aria-expanded]");

  function close() {
    const key = openKey;
    setOpenKey(null);
    if (key !== null) toggleOf(key)?.focus();
  }
  const toggle = (key: string) => { if (openKey === key) close(); else setOpenKey(key); };

  let body: ReactNode;
  if (isLoading) {
    body = <SkeletonRows n={6} />;
  } else if (error && !rows) {
    // A failed refresh keeps the rows already on screen; only a failed first load lands here.
    body = (
      <ErrorNotice action={<Button variant="secondary" size="sm" onClick={onRetry}>Retry</Button>}>
        Couldn't load {label.toLowerCase()}: {error instanceof ApiError ? error.message : "Unknown error"}
      </ErrorNotice>
    );
  } else if (!rows) {
    body = null;
  } else if (rows.length === 0) {
    body = empty;
  } else {
    const table = (
      <div className="table-wrap">
        <table className="data" aria-label={label} ref={tableRef}>
          <thead>
            <tr>
              {columns.map((c) => <th key={c.id} className={c.numeric ? "col-num" : undefined}>{c.header}</th>)}
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => {
              const key = rowKey(r);
              if (!detail) {
                return <tr key={key}>{columns.map((c) => <td key={c.id} className={c.numeric ? "col-num" : undefined}>{c.cell(r)}</td>)}</tr>;
              }
              const isOpen = openKey === key;
              return (
                <tr
                  key={key}
                  data-row-key={key}
                  className="clickable"
                  // A convenience for the pointer; the button in the first cell is the control.
                  onClick={(e) => { if (!(e.target as HTMLElement).closest("a, button, input, select")) toggle(key); }}
                >
                  {columns.map((c, i) => (
                    <td key={c.id} className={c.numeric ? "col-num" : undefined}>
                      {i === 0 ? (
                        <span className="row row-center gap-2">
                          <Button
                            variant="ghost"
                            size="sm"
                            iconOnly
                            icon={<ChevronRight size={14} aria-hidden="true" />}
                            aria-label={rowLabel ? `Show details for ${rowLabel(r)}` : "Show details"}
                            aria-expanded={isOpen}
                            aria-controls={isOpen ? panelId : undefined}
                            onClick={() => toggle(key)}
                          />
                          {c.cell(r)}
                        </span>
                      ) : c.cell(r)}
                    </td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    );
    body = (
      <>
        {/* One wrapper either way, so opening the panel does not remount the table. */}
        <div className={open ? "inspector-grid" : undefined}>
          {table}
          {open && (
            <div className="detail-pane" id={panelId} role="region" aria-label={`${label} details`} tabIndex={-1} ref={panelRef}>
              <div className="detail-toolbar">
                <Button variant="secondary" size="sm" onClick={close}>Close</Button>
              </div>
              {detail!(open)}
            </div>
          )}
        </div>
        {hasMore && onLoadMore && (
          <div className="load-more-row">
            <Button variant="secondary" size="sm" onClick={onLoadMore}>Load more</Button>
          </div>
        )}
      </>
    );
  }

  return (
    // Escape closes the detail from wherever the focus is: the row's button or the panel.
    <div
      className="log-view"
      role="group"
      aria-label={`${label} log`}
      tabIndex={-1}
      ref={rootRef}
      onKeyDown={(e) => { if (e.key === "Escape" && open) { e.stopPropagation(); close(); } }}
    >
      <div className="filter-row">
        <Segmented aria-label="Time range" options={TIME_RANGES} value={range} onChange={onRangeChange} />
        <Input
          type="search"
          aria-label="Filter"
          placeholder="Filter"
          className="fill-rest"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
        />
        {filters}
      </div>
      {body}
    </div>
  );
}
