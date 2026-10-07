import { useState, useEffect, useLayoutEffect, useRef } from "react";
import { createPortal } from "react-dom";
import type { ReactNode } from "react";
import { ChevronDown, Check } from "lucide-react";
import { placeMenu } from "./placeMenu";

export interface SelectOption {
  value: string;
  label: ReactNode;
}

export interface SelectProps {
  options: SelectOption[];
  value?: string;
  onChange?: (value: string) => void;
  placeholder?: string;
  id?: string;
  "aria-describedby"?: string;
}

export function Select({ options, value, onChange, placeholder = "Select…", id, "aria-describedby": describedBy }: SelectProps) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<{ top: number; left: number; width: number } | null>(null);

  // The list is portalled to <body> and positioned in viewport coordinates so
  // a scrolling ancestor (e.g. .dialog-body) can never clip it. It is at
  // least as wide as the trigger.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const t = triggerRef.current?.getBoundingClientRect();
      const m = listRef.current;
      if (!t || !m) return;
      const width = Math.max(m.offsetWidth, t.width);
      setPos({
        ...placeMenu(
          t,
          { width, height: m.offsetHeight },
          { width: window.innerWidth, height: window.innerHeight },
          "left",
        ),
        width: t.width,
      });
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
      // Next open starts hidden until it has been measured again.
      setPos(null);
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      const target = e.target as Node;
      if (triggerRef.current?.contains(target) || listRef.current?.contains(target)) return;
      setOpen(false);
    };
    // Capture phase + stopPropagation: Escape closes only the list, not a
    // surrounding Dialog that listens for the same key on document.
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      setOpen(false);
      triggerRef.current?.focus();
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey, true);
    };
  }, [open]);

  const selected = options.find((o) => o.value === value);
  return (
    <div style={{ position: "relative" }}>
      <button
        ref={triggerRef}
        id={id}
        type="button"
        className="select-trigger"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-describedby={describedBy}
        onClick={() => setOpen((o) => !o)}
      >
        <span className={selected ? "" : "placeholder"}>
          {selected ? selected.label : placeholder}
        </span>
        <ChevronDown size={14} />
      </button>
      {open && createPortal(
        <div
          ref={listRef}
          className="menu menu-enter select-list"
          role="listbox"
          // The list is not a DOM descendant of the trigger's container:
          // keep the browser from moving focus to <body> on mousedown.
          onMouseDown={(e) => e.preventDefault()}
          style={{
            position: "fixed",
            top: pos?.top ?? 0,
            left: pos?.left ?? 0,
            minWidth: pos?.width,
            visibility: pos ? "visible" : "hidden",
            // Above dialogs (z-index 30) so selects inside dialogs still work.
            zIndex: 40,
          }}
        >
          {options.map((o) => (
            <div
              key={o.value}
              role="option"
              aria-selected={o.value === value}
              className="menu-item"
              onClick={() => {
                triggerRef.current?.focus();
                onChange?.(o.value);
                setOpen(false);
              }}
            >
              {o.label}
              {o.value === value && <Check size={14} style={{ marginLeft: "auto" }} />}
            </div>
          ))}
        </div>,
        document.body,
      )}
    </div>
  );
}
