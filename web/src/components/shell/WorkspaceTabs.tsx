import { Link } from "react-router-dom";
import { cx } from "@/components/ds";
import type { Navigation } from "@/lib/navigation";
import { BurrowMark } from "./BurrowMark";

export interface WorkspaceTabsProps {
  /** The workspace whose navigation the sidebar shows; in Settings, neither tab. */
  current: Navigation;
  /** The workspaces this user may switch between. */
  workspaces: Navigation[];
  collapsed?: boolean;
}

/**
 * Head of the sidebar: the Burrow mark and, below it, one link per workspace,
 * each with its own icon and leading to that workspace's home. They are links
 * and not a tablist: choosing one opens another page. With a single workspace
 * there is nothing to choose, so the mark stands beside the workspace's name.
 */
export function WorkspaceTabs({ current, workspaces, collapsed = false }: WorkspaceTabsProps) {
  if (workspaces.length < 2) {
    return (
      <div className="workspace-brand">
        <BurrowMark />
        {!collapsed && <span className="workspace-title">{current.label}</span>}
      </div>
    );
  }

  return (
    <>
      <div className="workspace-brand"><BurrowMark /></div>
      <nav className="workspace-tabs" aria-label="Workspace">
        {workspaces.map((ws) => {
          const Icon = ws.icon;
          const active = ws.workspace === current.workspace;
          return (
            <Link
              key={ws.workspace}
              to={ws.home}
              className={cx("workspace-tab", active && "is-active")}
              aria-current={active ? "page" : undefined}
              aria-label={collapsed ? ws.label : undefined}
              title={collapsed ? ws.label : undefined}
            >
              <Icon size={15} aria-hidden="true" />
              {!collapsed && <span>{ws.label}</span>}
            </Link>
          );
        })}
      </nav>
    </>
  );
}
