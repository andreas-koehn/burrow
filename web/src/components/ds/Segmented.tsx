import { useRef } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { cx } from "./cx";

export interface SegmentedOption<T extends string> {
  value: T;
  label: ReactNode;
}

export interface SegmentedProps<T extends string> {
  /** Names the group for assistive technology, e.g. "Time range". */
  "aria-label": string;
  options: readonly SegmentedOption<T>[];
  value: T;
  onChange: (value: T) => void;
  className?: string;
}

/* ── Segmented ───────────────────────────────────────────────────
   A few exclusive choices in a toolbar: a radiogroup whose selected
   option is the only tab stop; the arrow keys move the choice and
   take the focus along, wrapping at either end. */
export function Segmented<T extends string>({ "aria-label": ariaLabel, options, value, onChange, className }: SegmentedProps<T>) {
  const ref = useRef<HTMLDivElement>(null);
  function onKeyDown(e: KeyboardEvent) {
    const step = e.key === "ArrowLeft" || e.key === "ArrowUp" ? -1 : e.key === "ArrowRight" || e.key === "ArrowDown" ? 1 : 0;
    if (step === 0) return;
    e.preventDefault();
    const at = options.findIndex((o) => o.value === value);
    const next = (at + step + options.length) % options.length;
    onChange(options[next].value);
    ref.current?.querySelectorAll<HTMLElement>('[role="radio"]')[next]?.focus();
  }
  return (
    <div ref={ref} role="radiogroup" aria-label={ariaLabel} className={cx("segmented", className)} onKeyDown={onKeyDown}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          role="radio"
          aria-checked={o.value === value}
          tabIndex={o.value === value ? 0 : -1}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}
