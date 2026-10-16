import { ALL_PERMISSIONS } from "@/auth/permissions";
import { Checkbox } from "@/components/ds";

/** The area of a permission is the part before its first colon; the label says it in words. */
const AREA_LABELS: Record<string, string> = {
  tunnels: "Service connections",
  tokens: "Client tokens",
  services: "Services",
  sessions: "Sessions",
  users: "Users",
  roles: "Roles",
  settings: "Settings",
  ai: "AI gateway",
  quotas: "Limits",
  inspector: "Inspector",
  audit: "Audit",
  webhooks: "Webhooks",
  automation: "Automation tokens",
  backup: "Backups",
  metrics: "Metrics",
  mcp: "MCP",
  mtls: "mTLS",
  ipgeo: "IP and country rules",
  budgets: "Budgets",
  cost: "Cost",
};

/** Groups in the order their first permission appears in ALL_PERMISSIONS; inside a group, that order too. */
const GROUPS: { area: string; label: string; permissions: string[] }[] = (() => {
  const byArea = new Map<string, string[]>();
  for (const p of ALL_PERMISSIONS) {
    const area = p.split(":")[0];
    byArea.set(area, [...(byArea.get(area) ?? []), p]);
  }
  return [...byArea].map(([area, permissions]) => ({ area, label: AREA_LABELS[area] ?? area, permissions }));
})();

export interface PermissionPickerProps {
  value: string[];
  onChange(next: string[]): void;
  /** What the reader holds; the rest is shown disabled. */
  held: ReadonlySet<string>;
  /** Keeps element ids apart when two pickers share a page. */
  idPrefix: string;
}

/** Permissions as checkboxes, grouped by area. The scroll region is the caller's choice of height (css). */
export function PermissionPicker({ value, onChange, held, idPrefix }: PermissionPickerProps) {
  const chosen = new Set(value);
  function toggle(p: string, on: boolean) {
    const next = new Set(chosen);
    if (on) next.add(p); else next.delete(p);
    onChange(ALL_PERMISSIONS.filter((x) => next.has(x)));
  }
  return (
    <div className="permission-picker">
      {GROUPS.map((g) => (
        <div key={g.area} role="group" aria-label={g.label} className="permission-group">
          <span className="permission-group-label muted small" aria-hidden="true">{g.label}</span>
          {g.permissions.map((p) => {
            const id = `${idPrefix}-${p.replaceAll(":", "-")}`;
            const mine = held.has(p);
            return (
              <div key={p} className="permission-option">
                <label className="checkbox-row small">
                  <Checkbox
                    id={id}
                    checked={chosen.has(p)}
                    disabled={!mine}
                    describedBy={mine ? undefined : `${id}-why`}
                    onChange={(on) => toggle(p, on)}
                  />
                  <span className="mono">{p}</span>
                </label>
                {!mine && <span id={`${id}-why`} className="muted small">You do not hold this.</span>}
              </div>
            );
          })}
        </div>
      ))}
    </div>
  );
}
