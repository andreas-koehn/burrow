import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { formatTimestamp } from "@/lib/format";
import { useAuth } from "@/auth/useAuth";
import { Button, FormField, FormFieldGroup, Input, PageHeader } from "@/components/ds";
import type { Session } from "@/lib/contract";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import { parseUserAgent } from "@/lib/userAgent";

function ActiveSessions() {
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({ queryKey: ["sessions"], queryFn: () => apiFetch<Session[]>("/sessions"), retry: false });
  const revoke = useMutation({
    mutationFn: (id: string) => apiFetch(`/sessions/${id}`, { method: "DELETE" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["sessions"] }); toast.success("Session revoked"); },
  });
  const revokeAll = useMutation({
    mutationFn: () => apiFetch("/sessions/revoke-all", { method: "POST" }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["sessions"] }); toast.success("Signed out everywhere"); },
  });
  return (
    <section className="account-section" aria-labelledby="sec-sessions">
      <div className="section-head">
        <div className="left"><h2 id="sec-sessions">Active sessions</h2></div>
        <Button variant="secondary" size="sm" onClick={() => revokeAll.mutate()}>Sign out everywhere</Button>
      </div>
      {isLoading ? <p className="muted">Loading…</p> : (
        <div className="table-wrap">
          <table className="data" aria-label="Active sessions">
            <thead><tr><th>Device</th><th>Created</th><th>Expires</th><th>IP</th><th className="col-actions">Actions</th></tr></thead>
            <tbody>
              {(data ?? []).map((s) => {
                const ua = parseUserAgent(s.user_agent ?? "");
                return (
                <tr key={s.id}>
                  <td>
                    <span title={s.user_agent}>{ua.browser} · {ua.os}</span>
                    {s.current && <span className="tag"> THIS DEVICE</span>}
                  </td>
                  <td className="col-created">{formatTimestamp(s.created_at)}</td>
                  <td className="col-created">{formatTimestamp(s.expires_at)}</td>
                  <td className="col-created">{s.ip}</td>
                  <td className="col-actions">
                    {s.current ? <span className="muted">Current session</span> :
                      <Button variant="destructive" size="sm" onClick={() => revoke.mutate(s.id)}>Revoke</Button>}
                  </td>
                </tr>
              );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function Account() {
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
    <div className="account-page">
      <PageHeader title="Account" />

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
      <ActiveSessions />
      <Toaster />
    </div>
  );
}
