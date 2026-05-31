import { useState, useId } from "react";
import type { ReactNode } from "react";
import { Info } from "lucide-react";
import { cx } from "./cx";

export interface InfoHintProps {
  label: string;
  content: ReactNode;
  className?: string;
}

export function InfoHint({ label, content, className }: InfoHintProps) {
  const id = useId();
  const [open, setOpen] = useState(false);

  return (
    <span className="info-hint-wrap">
      <button
        type="button"
        className={cx("info-hint", className)}
        aria-label={`What is ${label}?`}
        aria-describedby={id}
        onMouseEnter={() => setOpen(true)}
        onMouseLeave={() => setOpen(false)}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={(e) => {
          if (e.key === "Escape") setOpen(false);
        }}
      >
        <Info size={13} aria-hidden="true" />
      </button>
      <span role="tooltip" id={id} className="info-hint-pop" hidden={!open}>
        {content}
      </span>
    </span>
  );
}
