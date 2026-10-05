import { useId, useRef } from "react";
import type { KeyboardEvent, ReactNode } from "react";

export interface TabItem {
  value: string;
  label: ReactNode;
  content?: ReactNode;
}

export interface TabsProps {
  tabs: TabItem[];
  value: string;
  onChange: (value: string) => void;
}

export function Tabs({ tabs, value, onChange }: TabsProps) {
  const listRef = useRef<HTMLDivElement>(null);
  const uid = useId();
  // Selection follows focus: the arrow keys select the neighbouring tab and
  // move the focus to it, so the next arrow key starts from there.
  const go = (i: number) => {
    onChange(tabs[i].value);
    listRef.current?.querySelectorAll<HTMLElement>('[role="tab"]')[i]?.focus();
  };
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const i = tabs.findIndex((t) => t.value === value);
    if (e.key === "ArrowRight") {
      e.preventDefault();
      go((i + 1) % tabs.length);
    }
    if (e.key === "ArrowLeft") {
      e.preventDefault();
      go((i - 1 + tabs.length) % tabs.length);
    }
    if (e.key === "Home") {
      e.preventDefault();
      go(0);
    }
    if (e.key === "End") {
      e.preventDefault();
      go(tabs.length - 1);
    }
  };
  const at = tabs.findIndex((t) => t.value === value);
  const active = tabs[at];
  return (
    <div className="tabs">
      <div role="tablist" className="tabs-list" ref={listRef} onKeyDown={onKey}>
        {tabs.map((t, i) => (
          <button
            key={t.value}
            type="button"
            role="tab"
            id={`${uid}-tab-${i}`}
            aria-controls={`${uid}-panel`}
            aria-selected={t.value === value}
            tabIndex={t.value === value ? 0 : -1}
            className="tab"
            onClick={() => onChange(t.value)}
          >
            {t.label}
          </button>
        ))}
      </div>
      {/* One panel, named by whichever tab is selected. */}
      <div role="tabpanel" className="tab-panel" id={`${uid}-panel`} aria-labelledby={active ? `${uid}-tab-${at}` : undefined}>
        {active?.content}
      </div>
    </div>
  );
}
