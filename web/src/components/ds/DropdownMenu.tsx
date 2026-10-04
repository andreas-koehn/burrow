import { useState, useEffect, useLayoutEffect, useRef, cloneElement } from "react";
import { createPortal } from "react-dom";
import type { ReactElement, ReactNode, MouseEvent } from "react";
import { cx } from "./cx";
import { placeMenu } from "./placeMenu";

export interface DropdownItem {
  label?: string;
  onSelect?: () => void;
  danger?: boolean;
  icon?: ReactNode;
  shortcut?: string;
  sep?: boolean;
}

export interface DropdownMenuProps {
  trigger: ReactElement;
  items: DropdownItem[];
  align?: "left" | "right";
}

export function DropdownMenu({ trigger, items, align = "right" }: DropdownMenuProps) {
  const [open, setOpen] = useState(false);
  const [focusIdx, setFocusIdx] = useState(0);
  const wrapRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);

  // The menu is portalled to <body> and positioned in viewport coordinates so
  // an ancestor with overflow (e.g. .table-wrap) can never clip it.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const t = triggerRef.current?.getBoundingClientRect();
      const m = menuRef.current;
      if (!t || !m) return;
      setPos(placeMenu(
        t,
        { width: m.offsetWidth, height: m.offsetHeight },
        { width: window.innerWidth, height: window.innerHeight },
        align,
      ));
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
  }, [open, align]);

  // Focus goes back to the trigger before onSelect runs: a Dialog opened by
  // the item records document.activeElement and returns focus there on close.
  const select = (it: DropdownItem | undefined) => {
    triggerRef.current?.focus();
    it?.onSelect?.();
    setOpen(false);
  };

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: globalThis.MouseEvent) => {
      const target = e.target as Node;
      if (wrapRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setOpen(false);
        triggerRef.current?.focus();
      }
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setFocusIdx((i) => Math.min(items.length - 1, i + 1));
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setFocusIdx((i) => Math.max(0, i - 1));
      }
      if (e.key === "Enter") {
        e.preventDefault();
        select(items[focusIdx]);
      }
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open, items, focusIdx]);

  const injected: Record<string, unknown> = {
    ref: triggerRef,
    onClick: (e: MouseEvent) => {
      e.stopPropagation();
      setOpen((o) => !o);
      setFocusIdx(0);
    },
    "aria-haspopup": "menu",
    "aria-expanded": open,
  };

  return (
    <div ref={wrapRef} style={{ position: "relative", display: "inline-block" }}>
      {cloneElement(trigger, injected)}
      {open && createPortal(
        <div
          ref={menuRef}
          className="menu menu-enter"
          role="menu"
          style={{
            position: "fixed",
            top: pos?.top ?? 0,
            left: pos?.left ?? 0,
            visibility: pos ? "visible" : "hidden",
            // Above dialogs (z-index 30) so menus inside dialogs still work.
            zIndex: 40,
          }}
        >
          {items.map((it, i) =>
            it.sep ? (
              <div key={`s${i}`} className="menu-sep" />
            ) : (
              <div
                key={it.label}
                role="menuitem"
                className={cx("menu-item", it.danger && "danger", focusIdx === i && "is-focus")}
                onMouseEnter={() => setFocusIdx(i)}
                onClick={() => select(it)}
              >
                {it.icon && (
                  <span style={{ color: it.danger ? "inherit" : "var(--muted-foreground)" }}>
                    {it.icon}
                  </span>
                )}
                <span>{it.label}</span>
                {it.shortcut && <span className="shortcut">{it.shortcut}</span>}
              </div>
            ),
          )}
        </div>,
        document.body,
      )}
    </div>
  );
}
