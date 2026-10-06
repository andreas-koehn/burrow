import { useEffect, useRef, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { apiFetch, ApiError } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { Button, FormField, Input } from "@/components/ds";
import { BurrowMark } from "@/pages/Login";
import type { ClientLoginRequest } from "@/lib/contract";

const INVALID_CODE = "This code is not valid or has expired. Run burrow login again.";
const TOO_MANY = "Too many wrong codes. Wait a minute, then try again.";
const TOKEN_NAME_MAX = 120;
// The longest piece of text from the client that is shown; the rest is cut.
const SHOWN_MAX = 128;

/**
 * The code as the relay's path takes it: eight letters or digits, upper case,
 * without dash or blanks. Null for anything else, which is then never sent.
 */
function normalizeCode(raw: string): string | null {
  const code = raw.replace(/[-\s]/g, "").toUpperCase();
  return /^[A-Z0-9]{8}$/.test(code) ? code : null;
}

function clip(text: string): string {
  const chars = [...text];
  return chars.length > SHOWN_MAX ? `${chars.slice(0, SHOWN_MAX).join("")}…` : text;
}

function ageText(seconds: number): string {
  const minutes = Math.floor(seconds / 60);
  if (minutes < 1) return "less than a minute ago";
  return minutes === 1 ? "1 minute ago" : `${minutes} minutes ago`;
}

// A page inside somebody else's frame can be dressed up so that a click lands
// on Approve. The session cookie is not sent there anyway; this is the second lock.
function isFramed(): boolean {
  try {
    return window.top !== window.self;
  } catch {
    return true;
  }
}

function Shell({ children }: { children: ReactNode }) {
  return (
    <div className="signin-page">
      <main className="signin-container">
        <div className="signin-brand">
          <BurrowMark size={26} />
          <span>Burrow</span>
        </div>
        {children}
      </main>
    </div>
  );
}

/** What happened, said once and given the focus so that it is read out. */
function Result({ kind, children }: { kind: "status" | "alert"; children: ReactNode }) {
  const ref = useRef<HTMLParagraphElement>(null);
  useEffect(() => { ref.current?.focus(); }, []);
  return (
    <p ref={ref} tabIndex={-1} role={kind} className={`notice-inline linkpage-result ${kind === "status" ? "ok" : "error"}`}>
      <span className="body">{children}</span>
    </p>
  );
}

function InvalidCode() {
  return (
    <>
      <h1 className="signin-title">Sign in a machine</h1>
      <Result kind="alert">{INVALID_CODE}</Result>
      <p className="linkpage-next"><Link className="link-inline" to="/link">Enter another code</Link></p>
    </>
  );
}

function CodeEntry({ onCode }: { onCode: (code: string) => void }) {
  const [typed, setTyped] = useState("");
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => { field.current?.focus(); }, []);
  // Enter in the field ends here: it looks the request up and decides nothing.
  function submit(e: FormEvent) {
    e.preventDefault();
    const code = normalizeCode(typed);
    onCode(code ? `${code.slice(0, 4)}-${code.slice(4)}` : typed.trim());
  }
  return (
    <>
      <h1 className="signin-title">Sign in a machine</h1>
      <p className="signin-sub">burrow login shows a code in the terminal of the machine you are signing in.</p>
      <form onSubmit={submit} noValidate>
        <FormField
          label="Enter the code from your terminal"
          htmlFor="link-code"
          help="It looks like ABCD-EFGH. Small letters and a missing dash are fine."
        >
          <Input
            ref={field}
            id="link-code"
            name="code"
            mono
            maxLength={64}
            autoComplete="off"
            autoCapitalize="characters"
            autoCorrect="off"
            spellCheck={false}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        </FormField>
        <div className="signin-actions">
          <Button type="submit" variant="primary" className="signin-submit" disabled={typed.trim() === ""}>Continue</Button>
        </div>
      </form>
    </>
  );
}

type Outcome = "approved" | "denied" | "gone" | "forbidden";

function Request({ code }: { code: string }) {
  const qc = useQueryClient();
  const { user } = useAuth();
  const path = `/client/login/requests/${code}`;

  // The session ended: asking who is signed in again sends the visitor through the login and back here.
  function signedOut(e: unknown): boolean {
    if (!(e instanceof ApiError) || e.status !== 401) return false;
    void qc.invalidateQueries({ queryKey: ["me"] });
    return true;
  }

  // Looked up once. A wrong code counts against the account on the relay, so
  // nothing here asks a second time by itself; a decided request is not asked for again.
  const lookup = useQuery({
    queryKey: ["client-login-request", code],
    queryFn: async () => {
      try {
        return await apiFetch<ClientLoginRequest>(path);
      } catch (e) {
        signedOut(e);
        throw e;
      }
    },
    retry: false,
    staleTime: Infinity,
    gcTime: 0,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  });

  const [outcome, setOutcome] = useState<Outcome | null>(null);
  const [name, setName] = useState<string | null>(null);
  const [nameError, setNameError] = useState("");
  const [problem, setProblem] = useState("");
  // A second click arrives before the first one has disabled the button.
  const sent = useRef(false);

  function failed(e: unknown, what: "approve" | "deny") {
    sent.current = false;
    if (signedOut(e)) return;
    if (!(e instanceof ApiError)) return setProblem("The relay could not be reached. Try again.");
    switch (e.status) {
      case 404: // unknown or expired by now
      case 409: // decided in another tab or by somebody else
        return setOutcome("gone");
      case 429:
        return setProblem(TOO_MANY);
      case 403:
        if (/csrf/i.test(e.message)) return setProblem("This page is out of date. Reload it, then try again.");
        return setOutcome("forbidden");
      case 400:
        if (what === "approve") return setNameError(e.message);
        return setProblem(`Could not ${what} the request: ${e.message}`);
      default:
        return setProblem(`Could not ${what} the request: ${e.message}`);
    }
  }

  const approve = useMutation({
    mutationFn: (tokenName: string) =>
      apiFetch<ClientLoginRequest>(`${path}/approve`, { method: "POST", body: JSON.stringify({ token_name: tokenName }) }),
    onSuccess: () => setOutcome("approved"),
    onError: (e) => failed(e, "approve"),
  });
  const deny = useMutation({
    mutationFn: () => apiFetch<ClientLoginRequest>(`${path}/deny`, { method: "POST" }),
    onSuccess: () => setOutcome("denied"),
    onError: (e) => failed(e, "deny"),
  });

  const heading = useRef<HTMLHeadingElement>(null);
  const pending = lookup.data?.status === "pending" && outcome === null;
  // After the lookup the focus goes to the request, never to a button.
  useEffect(() => { if (pending) heading.current?.focus(); }, [pending]);

  if (outcome === "approved") {
    return (
      <>
        <h1 className="signin-title">Sign in a machine</h1>
        <Result kind="status">Approved. Your terminal will continue by itself. You can close this page.</Result>
        <p className="linkpage-next"><Link className="link-inline" to="/clients">See your clients</Link></p>
      </>
    );
  }
  if (outcome === "denied") {
    return (
      <>
        <h1 className="signin-title">Sign in a machine</h1>
        <Result kind="status">
          Denied. The sign-in request was discarded. To sign that machine in after all, run burrow login on it again.
        </Result>
        <p className="linkpage-next"><Link className="link-inline" to="/">Go to the dashboard</Link></p>
      </>
    );
  }
  if (outcome === "forbidden") {
    return (
      <>
        <h1 className="signin-title">Sign in a machine</h1>
        <Result kind="alert">
          Your account may not sign machines in. Ask an admin of this relay for the permission to manage client tokens.
        </Result>
        <p className="linkpage-next"><Link className="link-inline" to="/">Go to the dashboard</Link></p>
      </>
    );
  }
  if (outcome === "gone") return <InvalidCode />;

  if (lookup.isPending) {
    return (
      <>
        <h1 className="signin-title">Sign in a machine</h1>
        <p className="signin-sub">Loading…</p>
      </>
    );
  }
  if (lookup.isError) {
    const status = lookup.error instanceof ApiError ? lookup.error.status : 0;
    if (status === 404) return <InvalidCode />;
    if (status === 401) {
      return (
        <>
          <h1 className="signin-title">Sign in a machine</h1>
          <p className="signin-sub">Loading…</p>
        </>
      );
    }
    return (
      <>
        <h1 className="signin-title">Sign in a machine</h1>
        <Result kind="alert">{status === 429 ? TOO_MANY : "The sign-in request could not be loaded."}</Result>
        <div className="signin-actions">
          <Button type="button" className="signin-submit" onClick={() => void lookup.refetch()}>Try again</Button>
        </div>
      </>
    );
  }
  // Approved or denied already: the same answer as for a code that never existed.
  if (lookup.data.status !== "pending") return <InvalidCode />;

  const req = lookup.data;
  const tokenName = name ?? req.suggested_token_name;
  const trimmed = tokenName.trim();
  const tooLong = [...trimmed].length > TOKEN_NAME_MAX;
  const busy = approve.isPending || deny.isPending;
  const fieldError = tooLong ? `At most ${TOKEN_NAME_MAX} characters.` : nameError;

  function decide(what: "approve" | "deny") {
    if (sent.current) return;
    sent.current = true;
    setProblem("");
    setNameError("");
    if (what === "approve") approve.mutate(trimmed);
    else deny.mutate();
  }

  const said = (value: string) => (value.trim() === "" ? "not given" : clip(value));

  return (
    <>
      <h1 ref={heading} tabIndex={-1} className="signin-title">Sign in a machine</h1>
      <p className="signin-sub">A machine asks to be signed in to this relay.</p>

      <p className="linkpage-code mono">{req.user_code}</p>
      <p className="linkpage-compare">Check that your terminal shows the same code.</p>

      {/* Every value below is a text node. The first group is whatever the machine chose to say. */}
      <div role="group" aria-labelledby="link-reported" className="linkpage-facts">
        <p id="link-reported" className="linkpage-facts-title">Reported by the client</p>
        <dl>
          <dt>Hostname</dt><dd>{said(req.hostname)}</dd>
          <dt>Operating system</dt><dd>{said(req.os)}</dd>
          <dt>Architecture</dt><dd>{said(req.arch)}</dd>
          <dt>Client version</dt><dd>{said(req.client_version)}</dd>
        </dl>
      </div>
      <div role="group" aria-labelledby="link-seen" className="linkpage-facts">
        <p id="link-seen" className="linkpage-facts-title">Seen by the relay</p>
        <dl>
          <dt>Source IP</dt><dd className="mono">{said(req.source_ip)}</dd>
          <dt>Requested</dt><dd>{ageText(req.age_seconds)}</dd>
        </dl>
      </div>

      <p role="note" className="notice-inline warn linkpage-warning">
        <AlertTriangle size={14} className="icon" aria-hidden="true" />
        <span className="body">
          Approving gives that machine access to this relay as you{user ? ` (${user.email})` : ""}.{" "}
          If you did not just run burrow login, deny.
        </span>
      </p>

      <FormField
        label="Token name"
        htmlFor="link-token-name"
        help="The client token this creates is listed under Clients, tab Tokens, by this name."
        error={fieldError || undefined}
      >
        <Input
          id="link-token-name"
          name="token_name"
          autoComplete="off"
          spellCheck={false}
          invalid={fieldError !== ""}
          disabled={busy}
          value={tokenName}
          onChange={(e) => { setName(e.target.value); setNameError(""); }}
        />
      </FormField>

      {problem && <p role="alert" className="notice-inline error linkpage-problem"><span className="body">{problem}</span></p>}

      {/* Two plain buttons, no form: Enter in the field above decides nothing. */}
      <div className="linkpage-actions">
        <Button type="button" variant="secondary" disabled={busy} onClick={() => decide("deny")}>Deny</Button>
        <Button type="button" variant="primary" disabled={busy || trimmed === "" || tooLong} onClick={() => decide("approve")}>Approve</Button>
      </div>
    </>
  );
}

/**
 * /link?code=…: where `burrow login` sends the browser. Shows the machine
 * that asks to be signed in and lets the signed-in user approve or deny it.
 * Nothing is decided by opening the page; only the two buttons do that.
 */
export default function LinkClient() {
  const [params, setParams] = useSearchParams();
  const raw = params.get("code") ?? "";

  let content: ReactNode;
  if (isFramed()) {
    content = (
      <>
        <h1 className="signin-title">Sign in a machine</h1>
        <Result kind="alert">Open this page in its own browser tab to continue.</Result>
      </>
    );
  } else if (raw.trim() === "") {
    content = <CodeEntry onCode={(code) => setParams({ code })} />;
  } else {
    const code = normalizeCode(raw);
    content = code ? <Request key={code} code={code} /> : <InvalidCode />;
  }
  return <Shell>{content}</Shell>;
}
