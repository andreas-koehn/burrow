import { useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, FormField, FormFieldGroup, Input } from "@/components/ds";
import { returnPathFrom } from "@/lib/returnPath";

export function BurrowMark({ size = 26 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="3" y="5.5" width="12" height="13" rx="2.5" />
      <path d="M9 12h11.5" />
      <path d="M17 9l3.5 3-3.5 3" />
    </svg>
  );
}

export default function Login() {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState("");
  const nav = useNavigate();
  // Where RequireAuth sent the visitor away from; the start page otherwise.
  const back = returnPathFrom(useLocation().state);
  const qc = useQueryClient();
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setErr("");
    try {
      await apiFetch("/auth/login", { method: "POST", body: JSON.stringify({ email, password }) });
      await qc.invalidateQueries({ queryKey: ["me"] });
      nav(back, { replace: true });
    } catch (err: unknown) {
      if (err instanceof ApiError && err.status === 429) {
        setErr("Too many login attempts. Please wait a minute and try again.");
      } else {
        setErr("Invalid email or password");
      }
    }
  }
  return (
    <div className="signin-page">
      <div className="signin-container">
        <div className="signin-brand">
          <BurrowMark size={26} />
          <span>Burrow</span>
        </div>
        <h1 className="signin-title">Sign in to Burrow</h1>
        <p className="signin-sub">Use the credentials issued by your relay admin.</p>

        <form onSubmit={submit} noValidate>
          <FormFieldGroup>
            <FormField label="Email" htmlFor="login-email" w="full">
              <Input
                id="login-email"
                type="email"
                name="email"
                autoComplete="email"
                placeholder="you@example.com"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                aria-invalid={err ? true : undefined}
              />
            </FormField>
            <FormField label="Password" htmlFor="login-password" w="full">
              <Input
                id="login-password"
                type="password"
                name="password"
                autoComplete="current-password"
                placeholder="Password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                aria-invalid={err ? true : undefined}
              />
            </FormField>
          </FormFieldGroup>

          <div className="signin-actions">
            <Button type="submit" variant="primary" className="signin-submit">Sign in</Button>
          </div>

          {err && (
            <div className="signin-error" role="alert" aria-live="polite">
              <AlertTriangle size={14} className="icon" />
              <span>{err}</span>
            </div>
          )}
        </form>

        <p className="signin-footer">self-hosted relay · Apache-2.0 · no telemetry</p>
      </div>
    </div>
  );
}
