import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { AlertTriangle } from "lucide-react";
import { Button, Select, Switch, Badge, Dialog, FormField } from "@/components/ds";
import type { UserAdmin, UserRole, UserStatus } from "@/lib/contract";
import { toast } from "sonner";
import { useRoleChoices } from "./useRoleChoices";

export function EditUserDialog({ user, selfId, onClose }: { user: UserAdmin | null; selfId?: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [role, setRole] = useState<UserRole>("user");
  const [status, setStatus] = useState<UserStatus>("active");
  const choices = useRoleChoices(user !== null, user?.role);
  const isSelf = !!user && user.id === selfId;

  const [confirming, setConfirming] = useState<"suspend" | "delete" | null>(null);
  const suspending = !!user && !isSelf && status !== user.status && status === "suspended";

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
    <>
    <Dialog
      open={user !== null}
      onOpenChange={(o) => { if (!o) onClose(); }}
      title="Edit user"
      description={user?.email}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>Cancel</Button>
          <Button variant="primary" disabled={save.isPending} onClick={() => { if (suspending) setConfirming("suspend"); else save.mutate(); }}>Save changes</Button>
        </>
      }
    >
      <div className="pw-form">
        <FormField label="Role" htmlFor="eu-role" help={choices.describe(role)} descId="eu-role-help">
          <Select id="eu-role" aria-describedby={choices.describe(role) ? "eu-role-help" : undefined} options={choices.options} value={role} onChange={(v) => setRole(v as UserRole)} />
        </FormField>
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
          <span>Danger zone — removes the user, their tokens, and their access to services.</span>
          <Button
            variant="destructive" size="sm" className="ml-auto"
            disabled={isSelf || del.isPending}
            title={isSelf ? "You can't suspend or delete your own account." : undefined}
            onClick={() => setConfirming("delete")}
          >Delete user</Button>
        </div>
      </div>
    </Dialog>
      <Dialog
        open={confirming !== null}
        onOpenChange={(o) => { if (!o) setConfirming(null); }}
        title={confirming === "delete" ? "Delete user?" : "Suspend user?"}
        description={confirming === "delete"
          ? `${user?.email} will lose access immediately, and their tokens and services are removed. This cannot be undone.`
          : `${user?.email} will be signed out and cannot sign in until the account is active again.`}
        footer={
          <>
            <Button variant="secondary" onClick={() => setConfirming(null)}>Cancel</Button>
            <Button
              variant="destructive-solid"
              disabled={save.isPending || del.isPending}
              onClick={() => { const c = confirming; setConfirming(null); if (c === "delete") del.mutate(); else save.mutate(); }}
            >{confirming === "delete" ? "Delete user" : "Suspend user"}</Button>
          </>
        }
      >
        <div className="confirm-icon" aria-hidden="true"><AlertTriangle size={16} /></div>
      </Dialog>
    </>
  );
}
