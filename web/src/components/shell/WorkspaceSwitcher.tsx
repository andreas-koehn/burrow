import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { Check, ChevronsUpDown } from "lucide-react";
import { placeMenu } from "@/components/ds/placeMenu";
import type { Navigation } from "@/lib/navigation";
import { BurrowMark } from "./BurrowMark";

export interface WorkspaceSwitcherProps {
  /** The workspace the URL is in. */
  current: Navigation;
  /** The workspaces this user may switch between. */
  workspaces: Navigation[];
  collapsed?: boolean;
}

/**
 * Head of the sidebar: the Burrow mark, the workspace and the relay namespace it
 * manages. With more than one workspace it is a menu button. The design system's
 * DropdownMenu has neither `menuitemradio` items nor roving focus, so the small
 * menu lives here; it shares `placeMenu` and the `.menu` styles.
 */
export function WorkspaceSwitcher({ current, workspaces, collapsed = false }: WorkspaceSwitcherProps) {
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [pos, setPos] = useState<{ top: number; left: number } | null>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);

  const body = (
    <>
      <BurrowMark />
      {!collapsed && (
        <span className="workspace-text">
          <span className="workspace-name">{current.label}</span>
          {current.namespace && <span className="namespace">{current.namespace}</span>}
        </span>
      )}
    </>
  );

  // Portalled and placed in viewport coordinates, like DropdownMenu, so the
  // sidebar's overflow can never clip it.
  useLayoutEffect(() => {
    if (!open) return;
    const place = () => {
      const t = buttonRef.current?.getBoundingClientRect();
      const m = menuRef.current;
      if (!t || !m) return;
      setPos(placeMenu(
        t,
        { width: m.offsetWidth, height: m.offsetHeight },
        { width: window.innerWidth, height: window.innerHeight },
        "left",
      ));
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
      setPos(null);
    };
  }, [open]);

  const placed = pos !== null;
  // Once the menu is visible, focus starts on the current workspace.
  useEffect(() => {
    if (!placed) return;
    const items = menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitemradio"]');
    const checked = menuRef.current?.querySelector<HTMLElement>('[aria-checked="true"]');
    (checked ?? items?.[0])?.focus();
  }, [placed]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      const target = e.target as Node;
      if (buttonRef.current?.contains(target) || menuRef.current?.contains(target)) return;
      setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  if (workspaces.length < 2) {
    return <div className="workspace-switcher">{body}</div>;
  }

  const close = () => {
    setOpen(false);
    buttonRef.current?.focus();
  };
  const choose = (ws: Navigation) => {
    close();
    if (ws.workspace !== current.workspace) navigate(ws.home);
  };

  const onMenuKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = [...(menuRef.current?.querySelectorAll<HTMLElement>('[role="menuitemradio"]') ?? [])];
    const at = items.indexOf(document.activeElement as HTMLElement);
    const focus = (i: number) => items[(i + items.length) % items.length]?.focus();
    switch (e.key) {
      case "ArrowDown": e.preventDefault(); focus(at + 1); break;
      case "ArrowUp": e.preventDefault(); focus(at - 1); break;
      case "Home": e.preventDefault(); focus(0); break;
      case "End": e.preventDefault(); focus(items.length - 1); break;
      case "Enter":
      case " ":
        e.preventDefault();
        if (at >= 0) choose(workspaces[at]);
        break;
      case "Escape": e.preventDefault(); close(); break;
      // Not prevented: the menu is portalled to the end of <body>, so the focus goes back
      // to the button first and the browser's own Tab carries on from there.
      case "Tab": close(); break;
    }
  };

  return (
    <>
      <button
        ref={buttonRef}
        type="button"
        className="workspace-switcher"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={`Workspace: ${current.label}`}
        title={collapsed ? `Workspace: ${current.label}` : undefined}
        onClick={() => setOpen((o) => !o)}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" && !open) { e.preventDefault(); setOpen(true); }
        }}
      >
        {body}
        {!collapsed && <ChevronsUpDown className="chev" size={14} aria-hidden="true" />}
      </button>
      {open && createPortal(
        <div
          ref={menuRef}
          className="menu menu-enter"
          role="menu"
          aria-label="Workspace"
          onKeyDown={onMenuKey}
          style={{
            position: "fixed",
            top: pos?.top ?? 0,
            left: pos?.left ?? 0,
            visibility: pos ? "visible" : "hidden",
            zIndex: 40,
          }}
        >
          {workspaces.map((ws) => {
            const Icon = ws.icon;
            return (
              <div
                key={ws.workspace}
                role="menuitemradio"
                aria-checked={ws.workspace === current.workspace}
                tabIndex={-1}
                className="menu-item"
                onClick={() => choose(ws)}
              >
                <span style={{ color: "var(--muted-foreground)" }}><Icon size={15} aria-hidden="true" /></span>
                <span>{ws.label}</span>
                {ws.namespace && <span className="shortcut">{ws.namespace}</span>}
                {/* A slot of fixed width on every item, so the namespaces line up. */}
                <span className="menu-check">
                  {ws.workspace === current.workspace && <Check size={14} aria-hidden="true" />}
                </span>
              </div>
            );
          })}
        </div>,
        document.body,
      )}
    </>
  );
}
