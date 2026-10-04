import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Select, Switch, Badge, Dialog } from "@/components/ds";
import type { UserAdmin, UserRole, UserStatus } from "@/lib/contract";
import { toast } from "sonner";

export function EditUserDialog({ user, selfId, onClose }: { user: UserAdmin | null; selfId?: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [role, setRole] = useState<UserRole>("user");
  const [status, setStatus] = useState<UserStatus>("active");
  const isSelf = !!user && user.id === selfId;

  useEffect(() => { if (user) { setRole(user.role); setStatus(user.status); } }, [user]);

  const save = useMutation({
    mutationFn: () => {
      const patch: { role?: UserRole; status?: UserStatus } = {};
      if (user && role !== user.role) patch.role = role;
      if (user && !isSelf && status !== user.status) patch.status = status;
      return apiFetch(`/users/${user!.id}`, { method: "PATCH", body: JSON.stringify(patch) });
    },
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["users"] }); toast.success("User updated"); onClose(); },
    onError: (e: unknown) => toast.error(e instanceof ApiError ? e.message : "Failed to update user"),
  });

  const del = useMutation({
    mutationFn: () => apiFetch(`/users/${user!.id}`, { method: "DELETE" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["users"] }); toast.success("User deleted"); onClose(); },
    onError: (e: unknown) => toast.error(e instanceof ApiError ? e.message : "Failed to delete user"),
  });

  return (
    <Dialog
      open={user !== null}
      onOpenChange={(o) => { if (!o) onClose(); }}
      title="Edit user"
      description={user?.email}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate()}>Save changes</Button>
        </>
      }
    >
      <div className="pw-form">
        <div className="field">
          <label htmlFor="eu-role">Role</label>
          <Select id="eu-role" options={[{ value: "user", label: "User" }, { value: "admin", label: "Admin" }]} value={role} onChange={(v) => setRole(v as UserRole)} />
        </div>
        <div className="field">
          <label>Status</label>
          <div className="row row-center gap-2">
            <Switch
              aria-label="User status"
              checked={status === "active"}
              aria-disabled={isSelf}
              disabled={isSelf}
              title={isSelf ? "You can't suspend or delete your own account." : undefined}
              onChange={(c) => { if (!isSelf) setStatus(c ? "active" : "suspended"); }}
            />
            <Badge kind={status === "active" ? "status-connected" : "status-suspended-muted"}>{status === "active" ? "Active" : "Suspended"}</Badge>
          </div>
        </div>
        <div className="danger-zone row row-center gap-2">
          <span>Danger zone — removes the user, their tokens, and tunnel ACLs.</span>
          <Button
            variant="destructive" size="sm" className="ml-auto"
            disabled={isSelf || del.isPending}
            title={isSelf ? "You can't suspend or delete your own account." : undefined}
            onClick={() => del.mutate()}
          >Delete user</Button>
        </div>
      </div>
    </Dialog>
  );
}
