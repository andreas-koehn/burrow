import { useEffect, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { ApiError } from "@/lib/api";
import { Button, ErrorNotice, Input, SkeletonRows } from "@/components/ds";
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
  hasMore?: boolean;
  onLoadMore?: () => void;
}

function RangeControl({ range, onChange }: { range: TimeRange; onChange: (r: TimeRange) => void }) {
  const ref = useRef<HTMLDivElement>(null);
  function onKeyDown(e: KeyboardEvent) {
    const step = e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : 0;
    if (step === 0) return;
    e.preventDefault();
    const at = TIME_RANGES.findIndex((r) => r.value === range);
    const next = (at + step + TIME_RANGES.length) % TIME_RANGES.length;
    onChange(TIME_RANGES[next].value);
    // The selected option is the only tab stop, so the focus goes along with the choice.
    ref.current?.querySelectorAll<HTMLElement>('[role="radio"]')[next]?.focus();
  }
  return (
    <div ref={ref} role="radiogroup" aria-label="Time range" className="segmented" onKeyDown={onKeyDown}>
      {TIME_RANGES.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={o.value === range}
          tabIndex={o.value === range ? 0 : -1}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

/**
 * One log table for every scope: a filter bar (time range, search, the page's own
 * controls), then loading, error, empty or the rows, with an optional detail panel
 * beside them. The page owns the query and the filter state; nothing is fetched here.
 */
export function LogView<Row>({
  label, rows, rowKey, columns, isLoading, error, onRetry,
  range, onRangeChange, search, onSearchChange, filters, empty, detail, hasMore, onLoadMore,
}: LogViewProps<Row>) {
  const [openKey, setOpenKey] = useState<string | null>(null);
  const tableRef = useRef<HTMLTableElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const open = detail && openKey !== null ? rows?.find((r) => rowKey(r) === openKey) : undefined;

  // The panel follows the table in the page; taking the focus there spares a keyboard
  // user the remaining rows and lets a screen reader announce what opened.
  useEffect(() => {
    if (openKey !== null) panelRef.current?.focus();
  }, [openKey]);

  function close() {
    const key = openKey;
    setOpenKey(null);
    const row = [...(tableRef.current?.querySelectorAll<HTMLElement>("tbody tr") ?? [])].find((tr) => tr.dataset.rowKey === key);
    row?.focus();
  }

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
              const cells = columns.map((c) => <td key={c.id} className={c.numeric ? "col-num" : undefined}>{c.cell(r)}</td>);
              if (!detail) return <tr key={key}>{cells}</tr>;
              return (
                <tr
                  key={key}
                  data-row-key={key}
                  className="clickable"
                  tabIndex={0}
                  aria-selected={openKey === key}
                  onClick={() => setOpenKey(key)}
                  onKeyDown={(e) => {
                    // Keys pressed on a control inside the row belong to that control.
                    if (e.target !== e.currentTarget || (e.key !== "Enter" && e.key !== " ")) return;
                    e.preventDefault();
                    setOpenKey(key);
                  }}
                >
                  {cells}
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
            <div className="detail-pane" role="region" aria-label={`${label} details`} tabIndex={-1} ref={panelRef}>
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
    // Escape closes the detail from wherever the focus is: the row or the panel.
    <div className="log-view" onKeyDown={(e) => { if (e.key === "Escape" && open) { e.stopPropagation(); close(); } }}>
      <div className="filter-row">
        <RangeControl range={range} onChange={onRangeChange} />
        <Input
          type="search"
          aria-label="Filter"
          placeholder="Filter"
          className="flex-1"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
        />
        {filters}
      </div>
      {body}
    </div>
  );
}
