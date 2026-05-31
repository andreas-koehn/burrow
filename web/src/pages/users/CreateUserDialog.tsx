import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Input, Select, Dialog, FormField, FormFieldGroup } from "@/components/ds";
import type { UserAdmin, UserRole } from "@/lib/contract";
import { toast } from "sonner";

export function CreateUserDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<UserRole>("user");
  const [formError, setFormError] = useState("");

  function reset() { setEmail(""); setPassword(""); setRole("user"); setFormError(""); }

  const create = useMutation({
    mutationFn: () => apiFetch<UserAdmin>("/users", { method: "POST", body: JSON.stringify({ email, password, role }) }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["users"] }); toast.success("User created"); reset(); onClose(); },
    onError: (e: unknown) => {
      if (e instanceof ApiError && e.status === 409) setFormError("That email is already in use.");
      else if (e instanceof ApiError && e.status === 400) setFormError(e.message);
      else setFormError("Failed to create user.");
    },
  });

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o) { reset(); onClose(); } }}
      title="Create user"
      description="They'll be able to sign in to this relay. They sign in with the password you set here — Burrow does not email an invitation."
      footer={
        <>
          <Button variant="ghost" onClick={() => { reset(); onClose(); }}>Cancel</Button>
          <Button variant="primary" disabled={create.isPending} onClick={() => { setFormError(""); create.mutate(); }}>
            {create.isPending ? "Creating…" : "Create user"}
          </Button>
        </>
      }
    >
      <form className="pw-form" onSubmit={(e) => { e.preventDefault(); setFormError(""); create.mutate(); }}>
        <FormFieldGroup>
          <FormField label="Email" htmlFor="cu-email" w="md">
            <Input id="cu-email" type="email" autoComplete="off" value={email} invalid={!!formError} onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" />
          </FormField>
          <FormField label="Password" htmlFor="cu-pw" w="md" help="Minimum 8 characters. The user can change it after signing in.">
            <Input id="cu-pw" type="password" autoComplete="new-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </FormField>
          <FormField label="Role" htmlFor="cu-role" w="md">
            <Select id="cu-role" options={[{ value: "user", label: "User" }, { value: "admin", label: "Admin" }]} value={role} onChange={(v) => setRole(v as UserRole)} />
          </FormField>
        </FormFieldGroup>
        {formError && <p role="alert" className="field-error">{formError}</p>}
        <button type="submit" hidden />
      </form>
    </Dialog>
  );
}
