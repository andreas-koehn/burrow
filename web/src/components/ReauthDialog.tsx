import { useCallback, useEffect, useRef, useState } from "react";
import type { FormEvent } from "react";
import { apiFetch, ApiError, REAUTH_CANCELLED, setReauthHandler } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, Input } from "@/components/ds";

/** How long a confirmation keeps the relay's protected actions open (the relay's window). */
const WINDOW_MINUTES = 10;

interface Waiter {
  resolve: () => void;
  reject: (e: unknown) => void;
}

/**
 * The one "Confirm your password" dialog of the app. While the relay has no
 * separate services domain, tunnelled apps run on the dashboard's address and
 * could send any write as the signed-in person, so the relay asks for the
 * password again before it mints a secret or changes who may do what
 * (403 reauth_required). apiFetch hands such a call to this dialog, waits for
 * the confirmation and repeats the call once. Calls that arrive while the
 * dialog is open share it. Mount it once, near the root.
 */
export function ReauthDialog() {
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const waiters = useRef<Waiter[]>([]);

  const settle = useCallback((fail: unknown | null) => {
    const list = waiters.current;
    waiters.current = [];
    setOpen(false);
    setPassword("");
    setError("");
    setBusy(false);
    for (const w of list) {
      if (fail === null) w.resolve();
      else w.reject(fail);
    }
  }, []);

  useEffect(() => {
    setReauthHandler(
      () =>
        new Promise<void>((resolve, reject) => {
          waiters.current.push({ resolve, reject });
          setOpen(true);
        }),
    );
    return () => setReauthHandler(null);
  }, []);

  const cancel = useCallback(() => {
    if (busy) return;
    settle(new ApiError(403, "Password confirmation cancelled.", REAUTH_CANCELLED));
  }, [busy, settle]);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy || password === "") return;
    setBusy(true);
    setError("");
    try {
      await apiFetch("/auth/reauth", { method: "POST", body: JSON.stringify({ password }) });
      settle(null);
    } catch (err) {
      setBusy(false);
      if (err instanceof ApiError && err.status === 429) setError("Too many attempts. Wait a minute, then try again.");
      else if (err instanceof ApiError && err.status === 403) setError("The password is not correct.");
      else if (err instanceof ApiError && err.status === 401) settle(err); // signed out meanwhile
      else setError("The relay could not be reached. Try again.");
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o) cancel(); }}
      size="sm"
      title="Confirm your password"
      description="This action asks for your password again."
      footer={
        <>
          <Button type="button" variant="secondary" disabled={busy} onClick={cancel}>Cancel</Button>
          <Button type="submit" form="reauth-form" variant="primary" disabled={busy || password === ""}>
            {busy ? "Confirming…" : "Confirm"}
          </Button>
        </>
      }
    >
      <form id="reauth-form" onSubmit={(e) => void submit(e)}>
        <ErrorNotice variant="warn" role="note">
          This relay has no separate services domain, so the apps you expose run on the same address as this
          dashboard. To keep one of them from acting as you, creating keys and tokens, changing users and
          settings and ending sessions need your password. It stays confirmed for {WINDOW_MINUTES} minutes.
        </ErrorNotice>
        <FormField label="Your password" htmlFor="reauth-password" error={error || undefined}>
          <Input
            id="reauth-password"
            name="password"
            type="password"
            autoComplete="current-password"
            invalid={error !== ""}
            disabled={busy}
            value={password}
            onChange={(e) => { setPassword(e.target.value); setError(""); }}
          />
        </FormField>
      </form>
    </Dialog>
  );
}
