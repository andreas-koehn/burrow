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
// What Tab can stop on inside a dialog.
const TABBABLE_SELECTOR = [
  "a[href]",
  "button:not([disabled])",
  'input:not([type="hidden"]):not([disabled])',
  "select:not([disabled])",
  "textarea:not([disabled])",
  '[tabindex]:not([tabindex="-1"])',
].join(", ");
// Lists and menus a dialog's controls portal to <body>: focus in there still
// belongs to the dialog.
const PORTAL_SCOPE = '[role="listbox"], [role="menu"]';

// A radio group is one Tab stop: its checked radio, or any of them when none
// is checked. Unchecked radios of a group with a checked one are skipped.
function tabbable(root: HTMLElement): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(TABBABLE_SELECTOR)].filter((el) => {
    if (el.closest("[hidden], [inert]")) return false;
    if (el instanceof HTMLInputElement && el.type === "radio" && !el.checked && el.name) {
      return !root.querySelector(`input[type="radio"][name="${CSS.escape(el.name)}"]:checked`);
    }
    return true;
  });
}

// Keeps Tab inside the topmost open dialog: aria-modal alone does not stop
// the keyboard from walking into the page behind it.
function trapTab(e: KeyboardEvent, root: HTMLElement) {
  const roots = document.querySelectorAll("[data-dialog-root]");
  if (roots[roots.length - 1] !== root.parentElement) return;
  const active = document.activeElement;
  const inside = active instanceof Node && root.contains(active);
  if (!inside && active instanceof Element && active.closest(PORTAL_SCOPE)) return;
  const stops = tabbable(root);
  if (stops.length === 0) {
    e.preventDefault();
    root.focus();
    return;
  }
  const first = stops[0]!;
  const last = stops[stops.length - 1]!;
  if (!inside || active === root) {
    e.preventDefault();
    (e.shiftKey ? last : first).focus();
  } else if (e.shiftKey && active === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && active === last) {
    e.preventDefault();
    first.focus();
  }
}

// The opener a closing dialog just handed focus back to. A dialog opened from
// inside another dialog (create, then reveal the secret) records a button of
// the first dialog as its opener; that button is gone when the second dialog
// closes, so focus falls back to where the first dialog returned it.
// Only valid within that chain: the next interaction outside a dialog (see CHAIN_SCOPE) forgets
// it, so an unrelated dialog never jumps to a stale button and no detached
// node stays referenced.
let chainOpener: HTMLElement | null = null;
const CHAIN_EVENTS = ["pointerdown", "keydown", "focusin"] as const;
// Still "inside" the chain: the dialog wrapper (the backdrop is a sibling of
// [role="dialog"], so the wrapper is what counts) and the lists and menus a
// dialog's controls portal to <body>.
const CHAIN_SCOPE = '[data-dialog-root], [role="listbox"], [role="menu"]';

function forgetChainOpener(e?: Event) {
  if (e && e.target instanceof Element && e.target.closest(CHAIN_SCOPE)) return;
  // A backdrop mousedown moves focus to the dialog's nearest focusable
  // ancestor (the shell's <main tabIndex={-1}>); that is still the chain.
  if (e?.type === "focusin" && e.target instanceof Element && e.target.querySelector("[data-dialog-root]")) return;
  chainOpener = null;
  for (const type of CHAIN_EVENTS) document.removeEventListener(type, forgetChainOpener, true);
}

// Call after focusing el, so its own focusin does not forget it right away.
function rememberChainOpener(el: HTMLElement) {
  chainOpener = el;
  for (const type of CHAIN_EVENTS) document.addEventListener(type, forgetChainOpener, true);
}

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
      else if (e.key === "Tab" && ref.current) trapTab(e, ref.current);
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
      // the dialog was open, e.g. a dropdown item or the previous dialog's
      // submit button; fall back to the opener the previous dialog of the chain returned to.
      if (opener instanceof HTMLElement && opener !== document.body && opener.isConnected) {
        opener.focus();
        rememberChainOpener(opener);
      } else {
        const fallback = chainOpener;
        forgetChainOpener();
        if (fallback?.isConnected) fallback.focus({ preventScroll: true });
      }
    };
  }, [open]);

  if (!open) return null;
  return (
    <div
      data-dialog-root=""
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
