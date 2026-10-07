import { useState, useEffect, useId, useLayoutEffect, useRef } from "react";
import type { KeyboardEvent as ReactKeyboardEvent } from "react";
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
  /** The control's name where no label points at its id. */
  "aria-label"?: string;
  "aria-describedby"?: string;
}

export function Select({ options, value, onChange, placeholder = "Select…", id, "aria-label": ariaLabel, "aria-describedby": describedBy }: SelectProps) {
  const [open, setOpen] = useState(false);
  // The option the keyboard is on while the list is open. Focus stays on the
  // trigger (aria-activedescendant names the option), so a surrounding
  // dialog's focus trap and focus return are not disturbed.
  const [active, setActive] = useState(-1);
  const listId = useId();
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
  const optionId = (i: number) => `${listId}-opt-${i}`;

  const openList = () => {
    // Start on the chosen option, or the first.
    setActive(Math.max(0, options.findIndex((o) => o.value === value)));
    setOpen(true);
  };
  const choose = (i: number) => {
    const o = options[i];
    if (!o) return;
    triggerRef.current?.focus();
    onChange?.(o.value);
    setOpen(false);
  };

  // The option under the keyboard is kept in view in a long list.
  useEffect(() => {
    if (!open || active < 0) return;
    document.getElementById(optionId(active))?.scrollIntoView?.({ block: "nearest" });
    // optionId only depends on listId, which never changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, active]);

  // Arrow keys, Home and End move through the options; Enter or Space takes
  // the one the keyboard is on; Tab closes the list and moves on.
  const onTriggerKey = (e: ReactKeyboardEvent<HTMLButtonElement>) => {
    if (!open) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        openList();
      }
      return;
    }
    const last = options.length - 1;
    switch (e.key) {
      case "ArrowDown": e.preventDefault(); setActive((a) => Math.min(last, a + 1)); break;
      case "ArrowUp": e.preventDefault(); setActive((a) => Math.max(0, a - 1)); break;
      case "Home": e.preventDefault(); setActive(0); break;
      case "End": e.preventDefault(); setActive(last); break;
      case "Enter":
      case " ":
        // Not the button's own click, which would only close the list.
        e.preventDefault();
        if (active >= 0) choose(active);
        else setOpen(false);
        break;
      case "Tab": setOpen(false); break;
      default:
        // A letter or digit goes to the next option that starts with it.
        if (e.key.length === 1 && e.key !== " " && !e.ctrlKey && !e.metaKey && !e.altKey) {
          const starts = (i: number) => {
            const label = options[i]?.label;
            return typeof label === "string" && label.toLowerCase().startsWith(e.key.toLowerCase());
          };
          const order = options.map((_, i) => (active + 1 + i) % options.length);
          const hit = order.find(starts);
          if (hit !== undefined) setActive(hit);
        }
    }
  };

  return (
    <div style={{ position: "relative" }}>
      <button
        ref={triggerRef}
        id={id}
        type="button"
        // The select-only combobox pattern: the trigger keeps the focus and
        // names the option the keyboard is on (aria-activedescendant is not
        // supported on a plain button).
        role="combobox"
        className="select-trigger"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={open ? listId : undefined}
        aria-activedescendant={open && active >= 0 ? optionId(active) : undefined}
        aria-label={ariaLabel}
        aria-describedby={describedBy}
        onClick={() => { if (open) setOpen(false); else openList(); }}
        onKeyDown={onTriggerKey}
      >
        <span className={selected ? "" : "placeholder"}>
          {selected ? selected.label : placeholder}
        </span>
        <ChevronDown size={14} />
      </button>
      {open && createPortal(
        <div
          ref={listRef}
          id={listId}
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
          {options.map((o, i) => (
            <div
              key={o.value}
              id={optionId(i)}
              role="option"
              aria-selected={o.value === value}
              className={i === active ? "menu-item is-focus" : "menu-item"}
              // A moving pointer takes the highlight along. Not mouseenter: a
              // pointer that merely lies where the list opens must not move
              // the keyboard's place.
              onMouseMove={() => { if (active !== i) setActive(i); }}
              onClick={() => choose(i)}
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
