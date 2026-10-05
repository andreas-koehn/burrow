import { useRef } from "react";
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
  };
  const active = tabs.find((t) => t.value === value);
  return (
    <div className="tabs">
      <div role="tablist" className="tabs-list" ref={listRef} onKeyDown={onKey}>
        {tabs.map((t) => (
          <button
            key={t.value}
            role="tab"
            aria-selected={t.value === value}
            tabIndex={t.value === value ? 0 : -1}
            className="tab"
            onClick={() => onChange(t.value)}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div role="tabpanel" className="tab-panel">
        {active?.content}
      </div>
    </div>
  );
}
