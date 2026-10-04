import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Badge, Dialog, PageHeader, SkeletonRows } from "@/components/ds";
import { CustomRoleEditor } from "@/components/CustomRoleEditor";
import { Toaster } from "@/components/ui/sonner";
import type { RoleSummary, RoleDetail } from "@/lib/contract";

function RoleDetailDialog({ name, onClose }: { name: string | null; onClose: () => void }) {
  const { data } = useQuery({
    queryKey: ["role", name],
    queryFn: () => apiFetch<RoleDetail>(`/roles/${name}`),
    enabled: name !== null,
  });
  return (
    <Dialog
      open={name !== null}
      onOpenChange={(o) => { if (!o) onClose(); }}
      size="md"
      title={name ?? ""}
      description="Read-only — built-in role."
      footer={<Button variant="primary" onClick={onClose}>Close</Button>}
    >
      <div>
        <p className="muted" role="note">Permissions are fixed for built-in roles. Create a custom role to choose your own set.</p>
        <ul aria-label="Permissions" className="perm-list">
          {(data?.permissions ?? []).map((p) => (
            <li key={p}><code className="perm-key">{p}</code></li>
          ))}
        </ul>
      </div>
    </Dialog>
  );
}

export default function Roles() {
  const [viewName, setViewName] = useState<string | null>(null);
  // editor open state: null = closed; "" = new role; otherwise existing role name
  const [editorName, setEditorName] = useState<string | null>(null);
  const [editorOpen, setEditorOpen] = useState(false);
  const { data, isLoading, error } = useQuery({
    queryKey: ["roles"],
    queryFn: () => apiFetch<RoleSummary[]>("/roles"),
    retry: false,
  });

  function openNew() { setEditorName(null); setEditorOpen(true); }
  function openEdit(name: string) { setEditorName(name); setEditorOpen(true); }

  if (error) {
    return (
      <div className="users-page">
        <PageHeader title="Roles" />
        <div className="notice-block error">
          <div className="icon-bubble"><AlertTriangle size={18} /></div>
          <p role="alert">Failed to load roles: {error instanceof ApiError ? error.message : "Unknown error"}</p>
        </div>
      </div>
    );
  }

  return (
    <div className="users-page roles-page">
      <PageHeader
        title="Roles"
        subtitle="What each role is allowed to do."
        actions={<Button variant="primary" size="sm" onClick={openNew}>New role</Button>}
      />
      {isLoading ? (
        <div className="table-wrap"><SkeletonRows n={2} /></div>
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Roles">
            <thead><tr><th>Role</th><th>Description</th><th className="col-actions"></th></tr></thead>
            <tbody>
              {(data ?? []).map((r) => {
                const isBuiltin = r.builtin !== false;
                return (
                  <tr key={r.name}>
                    <td>
                      {r.name}{" "}
                      {isBuiltin
                        ? <Badge kind="" nodot>Built-in</Badge>
                        : <Badge kind="badge-custom" nodot>Custom</Badge>}
                    </td>
                    <td>{r.description}</td>
                    <td className="col-actions">
                      {isBuiltin
                        ? <Button variant="secondary" size="sm" onClick={() => setViewName(r.name)}>View</Button>
                        : <Button variant="secondary" size="sm" onClick={() => openEdit(r.name)}>Edit</Button>}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <RoleDetailDialog name={viewName} onClose={() => setViewName(null)} />
      <CustomRoleEditor
        open={editorOpen}
        roleName={editorName}
        onClose={() => setEditorOpen(false)}
      />
      <Toaster />
    </div>
  );
}
