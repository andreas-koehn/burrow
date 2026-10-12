import { useId } from "react";
import type { ReactNode } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";

export interface DisclosureProps {
  /** The button's words, e.g. "Advanced". */
  label: ReactNode;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  children: ReactNode;
}

/**
 * A part of a form that stays closed until it is asked for. The owner holds
 * the state, so it can open the part when a field in it needs attention.
 * Closed, the content is not rendered: its fields keep their values in the
 * owner's state.
 */
export function Disclosure({ label, open, onOpenChange, children }: DisclosureProps) {
  const bodyId = useId();
  return (
    <div className="disclosure">
      <button
        type="button"
        className="link small disclosure-toggle"
        aria-expanded={open}
        aria-controls={bodyId}
        onClick={() => onOpenChange(!open)}
      >
        {open ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
        {label}
      </button>
      {open && <div id={bodyId} className="disclosure-body">{children}</div>}
    </div>
  );
}
