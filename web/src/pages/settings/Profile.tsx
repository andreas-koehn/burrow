import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { Button, FormField, FormFieldGroup, Input, PageHeader } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";

export default function Profile() {
  const { user } = useAuth();
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [clientError, setClientError] = useState("");
  const [serverError, setServerError] = useState("");

  const changePw = useMutation({
    mutationFn: () =>
      apiFetch("/auth/change-password", {
        method: "POST",
        body: JSON.stringify({ current_password: currentPassword, new_password: newPassword }),
      }),
    onSuccess: () => {
      toast.success("Password changed successfully");
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
      setServerError("");
      setClientError("");
    },
    onError: (err: unknown) => {
      if (err instanceof ApiError) {
        if (err.status === 401) {
          // Expected form error: wrong current password. Do NOT navigate away.
          setServerError("Current password is incorrect");
        } else if (err.status === 400) {
          const msg = err.message?.toLowerCase() ?? "";
          setServerError(msg.includes("short") || msg.includes("password") ? "Password too short (minimum 8 characters)" : "Invalid request: " + err.message);
        } else {
          setServerError("An error occurred. Please try again.");
        }
      } else {
        setServerError("An error occurred. Please try again.");
      }
    },
  });

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault();
    setClientError("");
    setServerError("");
    if (newPassword !== confirmPassword) {
      setClientError("New passwords do not match");
      return;
    }
    if (newPassword.length < 8) {
      setClientError("New password must be at least 8 characters");
      return;
    }
    changePw.mutate();
  }

  const avatarInitial = (user?.email?.[0] ?? "U").toUpperCase();

  return (
    <div className="account-page page-narrow">
      <PageHeader title="Profile & password" />

      <section className="account-section" aria-labelledby="sec-profile">
        <div className="section-head">
          <div className="left">
            <h2 id="sec-profile">Profile</h2>
          </div>
        </div>
        <div className="profile-card">
          <span className="avatar">{avatarInitial}</span>
          <div className="body">
            <span className="email">{user?.email}</span>
            <div className="meta">
              <span><span className="k">Role</span> &nbsp;<span className="v">{user?.role}</span></span>
            </div>
          </div>
        </div>
      </section>

      <section className="account-section" aria-labelledby="sec-pw">
        <div className="section-head">
          <div className="left">
            <h2 id="sec-pw">Change password</h2>
          </div>
        </div>
        <form onSubmit={handleSubmit} className="pw-form">
          <FormFieldGroup>
            <FormField label="Current password" htmlFor="current-password" w="md">
              <Input
                id="current-password"
                type="password"
                autoComplete="current-password"
                value={currentPassword}
                onChange={(e) => setCurrentPassword(e.target.value)}
                required
              />
            </FormField>
            <FormField label="New password" htmlFor="new-password" w="md">
              <Input
                id="new-password"
                type="password"
                autoComplete="new-password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                required
              />
            </FormField>
            <FormField label="Confirm new password" htmlFor="confirm-password" w="md">
              <Input
                id="confirm-password"
                type="password"
                autoComplete="new-password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                required
              />
            </FormField>
          </FormFieldGroup>
          {clientError && (
            <p role="alert" className="field-error">{clientError}</p>
          )}
          {serverError && (
            <p role="alert" className="field-error">{serverError}</p>
          )}
          <div className="actions">
            <Button type="submit" variant="primary" disabled={changePw.isPending}>
              {changePw.isPending ? "Saving…" : "Change password"}
            </Button>
          </div>
        </form>
      </section>
      <Toaster />
    </div>
  );
}
