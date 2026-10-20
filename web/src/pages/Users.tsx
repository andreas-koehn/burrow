import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, ShieldAlert } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { RelativeTime } from "@/components/RelativeTime";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { Button, EmptyState, Input, Select, Badge, PageHeader, SkeletonRows } from "@/components/ds";
import type { UserAdmin, UsersPage, UserRole } from "@/lib/contract";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { CreateUserDialog } from "@/pages/users/CreateUserDialog";
import { EditUserDialog } from "@/pages/users/EditUserDialog";
import { DeleteUserConfirm } from "@/pages/users/DeleteUserConfirm";

const PAGE = 20;

// The role's badge shows the role's own name; the teal style is a cosmetic choice for the built-in admin.
const roleLabel = (role: string) => role.charAt(0).toUpperCase() + role.slice(1);
const ROLE_KIND: Record<string, "role-admin-teal" | "role-user"> = { admin: "role-admin-teal" };

export default function Users() {
  const { user: me } = useAuth();
  // POST, PATCH and DELETE /users: users:manage. users:read lists them.
  const canManage = useCan(P.usersManage);
  const qc = useQueryClient();
  const [q, setQ] = useState("");
  const [roleFilter, setRoleFilter] = useState<"" | UserRole>("");
  const [offset, setOffset] = useState(0);
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<UserAdmin | null>(null);
  const [confirmTarget, setConfirmTarget] = useState<UserAdmin | null>(null);

  const { data, isLoading, error } = useQuery({
    queryKey: ["users", q, offset],
    queryFn: () => apiFetch<UsersPage>(`/users?q=${encodeURIComponent(q)}&limit=${PAGE}&offset=${offset}`),
    retry: false,
  });

  const deleteUser = useMutation({
    mutationFn: (id: string) => apiFetch(`/users/${id}`, { method: "DELETE" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["users"] }); toast.success("User deleted"); },
    onError: (e: unknown) => toast.error(e instanceof ApiError ? e.message : "Failed to delete user"),
  });

  if (error instanceof ApiError && error.status === 403) {
    return (
      <div className="users-page settings-page">
        <PageHeader title="Users" />
        <div className="notice-block warn">
          <div className="icon-bubble"><ShieldAlert size={18} /></div>
          <p role="alert">Admin access required.</p>
        </div>
      </div>
    );
  }
  if (error) {
    return (
      <div className="users-page settings-page">
        <PageHeader title="Users" />
        <div className="notice-block error">
          <div className="icon-bubble"><AlertTriangle size={18} /></div>
          <p role="alert">Failed to load users: {error instanceof ApiError ? error.message : "Unknown error"}</p>
        </div>
      </div>
    );
  }

  const rows = (data?.users ?? []).filter((u) => (roleFilter ? u.role === roleFilter : true));
  const total = data?.total ?? 0;

  return (
    <div className="users-page settings-page">
      <PageHeader
        title="Users"
        subtitle="People who can sign in to this Burrow relay."
        actions={canManage ? <Button variant="primary" size="sm" onClick={() => setCreating(true)}>New user</Button> : undefined}
      />


      <div className="users-filter-row toolbar-row">
        <Input
          type="search"
          role="searchbox"
          aria-label="Search users by email"
          placeholder="search by email…"
          value={q}
          onChange={(e) => { setQ(e.target.value); setOffset(0); }}
        />
        <Select
          aria-label="Filter by role"
          options={[
            { value: "", label: "Role · All" },
            { value: "admin", label: "Role · Admin" },
            { value: "user", label: "Role · User" },
          ]}
          value={roleFilter}
          onChange={(v) => setRoleFilter(v as "" | UserRole)}
        />
        <span className="muted ml-auto">{total} total</span>
      </div>

      {isLoading ? (
        <div className="table-wrap"><SkeletonRows n={5} /></div>
      ) : rows.length === 0 ? (
        <EmptyState title="No users found" />
      ) : (
        <div className="table-wrap">
          <table className="data" aria-label="Users">
            <thead>
              <tr><th>User</th><th>Role</th><th>Status</th><th>Created</th><th>Last login</th>{canManage && <th className="col-actions" aria-label="Row actions"></th>}</tr>
            </thead>
            <tbody>
              {rows.map((u) => (
                <tr key={u.id}>
                  <td>{u.email}{u.id === me?.id && <span className="tag" aria-label="this is you"> YOU</span>}</td>
                  <td><Badge kind={ROLE_KIND[u.role] ?? "role-user"}>{roleLabel(u.role)}</Badge></td>
                  <td><Badge kind={u.status === "active" ? "status-connected" : "status-suspended-muted"}>{u.status === "active" ? "Active" : "Suspended"}</Badge></td>
                  <td className="col-created"><RelativeTime value={u.created_at} /></td>
                  <td className="col-created">{u.last_login ? <RelativeTime value={u.last_login} /> : <span className="muted">—</span>}</td>
                  {canManage && <td className="col-actions">
                    <Button variant="secondary" size="sm" onClick={() => setEditing(u)}>Edit</Button>{" "}
                    <Button
                      variant="destructive" size="sm"
                      title={u.id === me?.id ? "You can't delete your own account." : undefined}
                      aria-label={`Delete user ${u.email}`}
                      disabled={u.id === me?.id}
                      onClick={() => setConfirmTarget(u)}
                    >Delete</Button>
                  </td>}
                </tr>
              ))}
            </tbody>
          </table>
          {total > PAGE && (
            <div className="row row-center gap-2 pagination-row">
              <span className="muted">Showing {offset + 1}–{Math.min(offset + PAGE, total)} of {total}</span>
              <div className="row row-center gap-2 ml-auto">
                <Button variant="secondary" size="sm" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - PAGE))}>Prev</Button>
                <Button variant="secondary" size="sm" disabled={offset + PAGE >= total} onClick={() => setOffset(offset + PAGE)}>Next</Button>
              </div>
            </div>
          )}
        </div>
      )}

      {canManage && <CreateUserDialog open={creating} onClose={() => setCreating(false)} />}
      {canManage && <EditUserDialog user={editing} selfId={me?.id} onClose={() => setEditing(null)} />}

      {confirmTarget && (
        <DeleteUserConfirm
          email={confirmTarget.email}
          pending={deleteUser.isPending}
          onCancel={() => setConfirmTarget(null)}
          onConfirm={() => { deleteUser.mutate(confirmTarget.id); setConfirmTarget(null); }}
        />
      )}
      <Toaster />
    </div>
  );
}
