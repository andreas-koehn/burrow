import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, Input, Switch, Tabs } from "@/components/ds";
import type { PermissionDef, RoleDetail } from "@/lib/contract";

export interface CustomRoleEditorProps {
  open: boolean;
  /** null = "new role" mode; otherwise editing an existing custom role */
  roleName: string | null;
  onClose: () => void;
}

export function CustomRoleEditor({ open, roleName, onClose }: CustomRoleEditorProps) {
  const qc = useQueryClient();
  const isNew = roleName === null;
  const perms = useQuery({
    queryKey: ["roles", "permissions"],
    queryFn: () => apiFetch<PermissionDef[]>("/roles/permissions"),
    retry: false,
    enabled: open,
  });
  const existing = useQuery({
    queryKey: ["role", roleName],
    queryFn: () => apiFetch<RoleDetail>(`/roles/${roleName}`),
    retry: false,
    enabled: open && roleName !== null,
  });

  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [defaultForNewUsers, setDefaultForNewUsers] = useState(false);
  const [granted, setGranted] = useState<Set<string>>(new Set());
  const [tab, setTab] = useState("general");
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (open && existing.data) {
      setName(existing.data.name);
      setDescription(existing.data.description);
      setGranted(new Set(existing.data.permissions));
    } else if (open && isNew) {
      setName("");
      setDescription("");
      setGranted(new Set());
    }
  }, [open, existing.data, isNew]);

  const grouped = useMemo(() => {
    const out: Record<string, PermissionDef[]> = {};
    for (const p of perms.data ?? []) (out[p.group] ||= []).push(p);
    return out;
  }, [perms.data]);

  const save = useMutation<RoleDetail | void, unknown, void>({
    mutationFn: async () => {
      if (isNew) {
        return apiFetch<RoleDetail>("/roles", {
          method: "POST",
          body: JSON.stringify({
            name, description,
            permissions: Array.from(granted),
            default_for_new_users: defaultForNewUsers,
          }),
        });
      }
      return apiFetch<void>(`/roles/${roleName}`, {
        method: "PUT",
        body: JSON.stringify({
          description,
          permissions: Array.from(granted),
          default_for_new_users: defaultForNewUsers,
        }),
      });
    },
    onSuccess: () => {
      toast.success(isNew ? "Role created." : "Role saved.");
      qc.invalidateQueries({ queryKey: ["roles"] });
      if (!isNew) qc.invalidateQueries({ queryKey: ["role", roleName] });
      onClose();
    },
    onError: (e: unknown) =>
      setErr(e instanceof ApiError ? e.message : "Couldn't save role."),
  });

  const remove = useMutation({
    mutationFn: () => apiFetch<void>(`/roles/${roleName}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success("Role deleted.");
      qc.invalidateQueries({ queryKey: ["roles"] });
      setDeleteOpen(false);
      onClose();
    },
    onError: (e: unknown) =>
      setErr(e instanceof ApiError ? e.message : "Couldn't delete role."),
  });

  function toggle(key: string, on: boolean) {
    setGranted((g) => {
      const next = new Set(g);
      if (on) next.add(key); else next.delete(key);
      return next;
    });
  }

  const summary = useMemo(() => {
    const lines: string[] = [];
    for (const [group, items] of Object.entries(grouped)) {
      const onCount = items.filter((p) => granted.has(p.key)).length;
      if (onCount > 0) lines.push(`${onCount} of ${items.length} ${group}`);
    }
    return lines.length === 0
      ? "No permissions granted yet."
      : `Grants ${lines.join(", ")}.`;
  }, [grouped, granted]);

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={(o) => { if (!o) onClose(); }}
        size="md"
        title={isNew ? "New role" : `Edit role · ${roleName ?? ""}`}
        footer={
          <>
            {!isNew && (
              <Button variant="destructive" onClick={() => setDeleteOpen(true)}>
                Delete role
              </Button>
            )}
            <Button variant="secondary" onClick={onClose}>Cancel</Button>
            <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>
              {isNew ? "Create" : "Save"}
            </Button>
          </>
        }
      >
        <Tabs
          value={tab}
          onChange={setTab}
          tabs={[
            { value: "general", label: "General", content: (
              <div>
                <div className="field">
                  <label htmlFor="role-name">Name</label>
                  <Input id="role-name" value={name} onChange={(e) => setName(e.target.value)} disabled={!isNew} />
                </div>
                <div className="field">
                  <label htmlFor="role-desc">Description</label>
                  <Input id="role-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
                </div>
                <div className="row row-center gap-2">
                  <Switch
                    aria-label="Default for new users"
                    checked={defaultForNewUsers}
                    onChange={setDefaultForNewUsers}
                  />
                  <span>Default for new users</span>
                </div>
                {defaultForNewUsers && (
                  <p className="muted">Only one role can be the default; setting this clears the previous default.</p>
                )}
              </div>
            ) },
            { value: "permissions", label: "Permissions", content: (
              <div>
                <p className="muted">{summary}</p>
                {Object.entries(grouped).map(([group, items]) => (
                  <div key={group} className="perm-group">
                    <h4>{group}</h4>
                    {items.map((p) => (
                      <div key={p.key} className="row row-center gap-2">
                        <Switch
                          aria-label={p.key}
                          checked={granted.has(p.key)}
                          onChange={(v) => toggle(p.key, v)}
                        />
                        <code className="mono small">{p.key}</code>
                        <span className="muted">{p.description}</span>
                      </div>
                    ))}
                  </div>
                ))}
              </div>
            ) },
            { value: "services", label: "Service access", content: (
              <p className="muted">Per-service access is managed on each burrow_login service.</p>
            ) },
          ]}
        />
        {err && <p role="alert" className="notice-inline error">{err}</p>}
      </Dialog>

      <Dialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={`Delete role '${roleName ?? ""}'?`}
        footer={
          <>
            <Button variant="secondary" onClick={() => setDeleteOpen(false)}>Cancel</Button>
            <Button variant="destructive" disabled={remove.isPending} onClick={() => remove.mutate()}>
              Delete
            </Button>
          </>
        }
      >
        <p>
          Delete role '{roleName}'? Users with this role will fall back to the User
          default role.
        </p>
      </Dialog>
    </>
  );
}
