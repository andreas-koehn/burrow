import { useEffect, useId, useRef } from "react";
import type { ReactNode } from "react";
import { X } from "lucide-react";
import { cx } from "./cx";

export type DialogSize = "sm" | "md" | "lg";

export interface DialogProps {
  open: boolean;
  onOpenChange?: (open: boolean) => void;
  title?: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  /** sm 420px (confirmations, short forms), md 560px (lists, tabs), lg 720px (access configuration). */
  size?: DialogSize;
}

const FIELD_SELECTOR =
  'input:not([type="hidden"]):not([disabled]), select:not([disabled]), textarea:not([disabled])';
const FOOTER_SELECTOR = ".dialog-footer button:not([disabled])";

export function Dialog({ open, onOpenChange, title, description, children, footer, size = "sm" }: DialogProps) {
  const ref = useRef<HTMLDivElement>(null);
  // Unique per-instance id so nested dialogs don't share aria-labelledby —
  // duplicate IDs were causing assistive tech and Playwright's
  // getByRole("dialog", { name }) to resolve to the wrong dialog.
  const titleId = useId();
  // Keep the latest onOpenChange without making it an effect dependency:
  // callers commonly pass a fresh closure every render, and re-running the
  // focus effect on every render would re-fire the initial-focus timer
  // and steal focus back from whatever the user is typing into.
  const onOpenChangeRef = useRef(onOpenChange);
  onOpenChangeRef.current = onOpenChange;
  // Remember what opened the dialog so focus can go back there on close.
  // Captured during the render that opens it: a child with autoFocus takes
  // focus at commit, before any effect here runs, so an effect would record
  // the dialog's own field as the opener. undefined = not captured yet.
  const openerRef = useRef<Element | null | undefined>(undefined);
  if (!open) openerRef.current = undefined;
  else if (openerRef.current === undefined) openerRef.current = document.activeElement;
  useEffect(() => {
    if (!open) return;
    const opener = openerRef.current;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onOpenChangeRef.current?.(false);
    };
    document.addEventListener("keydown", onKey);
    // Initial focus, once, on open: first form field in the body, else the
    // first footer button (Cancel in confirmations), else the dialog itself.
    // Never the close button, and never a body button — focusing the first
    // access-mode card made it look selected.
    const t = setTimeout(() => {
      const root = ref.current;
      if (!root || root.contains(document.activeElement)) return;
      const target =
        root.querySelector<HTMLElement>(`.dialog-body ${FIELD_SELECTOR}`) ??
        root.querySelector<HTMLElement>(FOOTER_SELECTOR) ??
        root;
      target.focus();
    }, 50);
    return () => {
      document.removeEventListener("keydown", onKey);
      clearTimeout(t);
      // Skip <body> (nothing was focused) and openers that unmounted while
      // the dialog was open, e.g. a dropdown item.
      if (opener instanceof HTMLElement && opener !== document.body && opener.isConnected) opener.focus();
    };
  }, [open]);

  if (!open) return null;
  return (
    <div
      style={{
        position: "fixed",
        inset: 0,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        zIndex: 30,
        padding: 16,
      }}
    >
      <div className="dialog-backdrop" onClick={() => onOpenChange?.(false)} />
      <div
        ref={ref}
        className={cx("dialog", `size-${size}`)}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
      >
        <div className="dialog-header">
          <h3 id={titleId}>{title}</h3>
          {description && <p>{description}</p>}
          {onOpenChange && (
            <button
              type="button"
              className="icon-btn dialog-close"
              aria-label="Close dialog"
              onClick={() => onOpenChange(false)}
            >
              <X size={14} />
            </button>
          )}
        </div>
        <div className="dialog-body">{children}</div>
        {footer && <div className="dialog-footer">{footer}</div>}
      </div>
    </div>
  );
}
