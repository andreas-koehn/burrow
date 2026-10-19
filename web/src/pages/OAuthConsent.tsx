import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { apiFetch, ApiError, REAUTH_CANCELLED } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import { Button, Checkbox, FormField, Input, Switch } from "@/components/ds";
import { Result, Shell } from "@/components/ApprovalShell";
import { isFramed } from "@/lib/framed";
import { goTo } from "@/lib/navigate";
import { isBroadScopes, resourceLabel, SCOPE_READ, SCOPE_WRITE } from "@/lib/oauth";
import type { OAuthApprove, OAuthRedirect, OAuthRequest } from "@/lib/contract";

const GONE = "This request is not valid or has expired. Go back to the app and start the connection again.";
// The longest piece of text from the app that is shown; the rest is cut.
const SHOWN_MAX = 128;

function clip(text: string): string {
  const chars = [...text];
  return chars.length > SHOWN_MAX ? `${chars.slice(0, SHOWN_MAX).join("")}…` : text;
}

function Gone() {
  return (
    <>
      <h1 className="signin-title">Connect an app</h1>
      <Result kind="alert">{GONE}</Result>
      <p className="linkpage-next"><Link className="link-inline" to="/">Go to the dashboard</Link></p>
    </>
  );
}

function Consent({ id }: { id: string }) {
  const qc = useQueryClient();
  const { user } = useAuth();
  const path = `/oauth/requests/${encodeURIComponent(id)}`;

  function signedOut(e: unknown): boolean {
    if (!(e instanceof ApiError) || e.status !== 401) return false;
    void qc.invalidateQueries({ queryKey: ["me"] });
    return true;
  }

  // Looked up once; a request is decided once, so nothing asks again by itself.
  const lookup = useQuery({
    queryKey: ["oauth-request", id],
    queryFn: async () => {
      try {
        return await apiFetch<OAuthRequest>(path);
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

  // null = untouched: the defaults come from the request.
  const [readOnly, setReadOnly] = useState<boolean | null>(null);
  const [sources, setSources] = useState<string[] | null>(null);
  const [password, setPassword] = useState("");
  const [problem, setProblem] = useState("");
  const [gone, setGone] = useState(false);
  // Set once the relay answered with the redirect: the browser is on its way
  // to the app, so nothing on this page may be pressed again.
  const [leaving, setLeaving] = useState(false);
  // A second click arrives before the first one has disabled the button.
  const sent = useRef(false);

  function leave(r: OAuthRedirect) {
    setLeaving(true);
    goTo(r.redirect_url);
  }

  function failed(e: unknown, what: "approve" | "deny") {
    sent.current = false;
    if (signedOut(e)) return;
    if (!(e instanceof ApiError)) return setProblem("The relay could not be reached. Try again.");
    if (e.status === 404) return setGone(true);
    if (e.status === 429) return setProblem("Too many attempts. Wait a minute, then try again.");
    if (e.status === 403 && e.code === REAUTH_CANCELLED) return setProblem("Approving needs your password. Press Approve to enter it.");
    if (e.status === 403 && /csrf/i.test(e.message)) return setProblem("This page is out of date. Reload it, then try again.");
    setProblem(`Could not ${what} the request: ${e.message}`);
  }

  const approve = useMutation({
    // While the relay has no services domain the password is sent with the
    // decision; a recent confirmation (apiFetch's dialog) does as well.
    mutationFn: (b: OAuthApprove) => apiFetch<OAuthRedirect>(`${path}/approve`, { method: "POST", body: JSON.stringify(b) }),
    onSuccess: leave,
    onError: (e) => failed(e, "approve"),
  });
  const deny = useMutation({
    mutationFn: () => apiFetch<OAuthRedirect>(`${path}/deny`, { method: "POST" }),
    onSuccess: leave,
    onError: (e) => failed(e, "deny"),
  });

  const heading = useRef<HTMLHeadingElement>(null);
  const ready = lookup.isSuccess && !gone;
  useEffect(() => { if (ready) heading.current?.focus(); }, [ready]);

  if (gone) return <Gone />;
  if (lookup.isPending) {
    return (
      <>
        <h1 className="signin-title">Connect an app</h1>
        <p className="signin-sub">Loading…</p>
      </>
    );
  }
  if (lookup.isError) {
    const status = lookup.error instanceof ApiError ? lookup.error.status : 0;
    if (status === 404) return <Gone />;
    if (status === 401) return <><h1 className="signin-title">Connect an app</h1><p className="signin-sub">Loading…</p></>;
    return (
      <>
        <h1 className="signin-title">Connect an app</h1>
        <Result kind="alert">
          {status === 403 ? "Your account may not connect apps." : "The request could not be loaded."}
        </Result>
        {status !== 403 && (
          <div className="signin-actions">
            <Button type="button" className="signin-submit" onClick={() => void lookup.refetch()}>Try again</Button>
          </div>
        )}
      </>
    );
  }

  const req = lookup.data;
  const burrow = req.resource_kind === "burrow";
  const broad = burrow && isBroadScopes(req.scopes);
  // The switch only ever narrows: an app that asked to read gets no way to
  // write from this page.
  const askedWrite = req.scopes.includes(SCOPE_WRITE);
  const ro = readOnly ?? (burrow ? !askedWrite : true);
  const chosen = sources ?? req.sources.map((s) => s.slug);
  const toolsBlocked = !burrow && !req.can_grant_tools;
  const noSource = !burrow && chosen.length === 0;
  const busy = approve.isPending || deny.isPending || leaving;
  const needsPassword = req.password_required && password === "";

  function decide(what: "approve" | "deny") {
    if (sent.current) return;
    sent.current = true;
    setProblem("");
    if (what === "deny") return deny.mutate();
    const b: OAuthApprove = {};
    if (burrow) {
      if (broad) b.scopes = [ro || !askedWrite ? SCOPE_READ : SCOPE_WRITE];
    } else {
      // Keep the relay's order of sources, whatever order the boxes were ticked in.
      b.allowed_tools = req.sources.filter((s) => chosen.includes(s.slug)).map((s) => `${s.slug}__*`);
      b.tools_read_only = ro;
    }
    if (req.password_required) b.password = password;
    approve.mutate(b);
  }

  const resource = resourceLabel(req.resource, req.sources.find((s) => s.slug === req.source)?.name);
  const name = clip(req.client_name);

  return (
    <>
      <h1 ref={heading} tabIndex={-1} className="signin-title">{name} wants to connect</h1>
      <p className="signin-sub">An app asks to use this relay as you{user ? ` (${user.email})` : ""}.</p>

      {/* Every value below is a text node. The app chose its own name; the relay read the address. */}
      <p className="consent-returns">
        After you decide, your browser goes to <strong className="mono consent-host">{req.redirect_host}</strong>
      </p>
      <div role="group" aria-labelledby="oc-facts" className="linkpage-facts">
        <p id="oc-facts" className="linkpage-facts-title">The request</p>
        <dl>
          <dt>App</dt><dd>{name}</dd>
          <dt>Access to</dt><dd>{resource}</dd>
        </dl>
      </div>

      {req.localhost_redirect && (
        <p role="note" className="notice-inline warn linkpage-warning">
          <AlertTriangle size={14} className="icon" aria-hidden="true" />
          <span className="body">
            This app returns to {req.redirect_host}, which is only this machine. Approve only if you started the
            connection from an app running on this computer.
          </span>
        </p>
      )}

      {burrow && (broad ? (askedWrite ? (
        <div className="form-field">
          <label htmlFor="oc-readonly" className="checkbox-row">
            <Switch id="oc-readonly" aria-label="Read-only access" checked={ro} disabled={busy} onChange={setReadOnly} />
            <span>Read-only access</span>
          </label>
          <p className="help">
            {ro
              ? "The app can look at everything your role can see and change nothing."
              : "The app can do everything your role can do, including changes."}
          </p>
        </div>
      ) : (
        <div role="group" aria-labelledby="oc-access" className="linkpage-facts">
          <p id="oc-access" className="linkpage-facts-title">Read-only access</p>
          <p className="help">The app asked to read only. It can look at everything your role can see and change nothing.</p>
        </div>
      )) : (
        <div role="group" aria-labelledby="oc-perms" className="linkpage-facts">
          <p id="oc-perms" className="linkpage-facts-title">Permissions asked for</p>
          <ul>{req.scopes.map((s) => <li key={s} className="mono">{s}</li>)}</ul>
        </div>
      ))}

      {!burrow && (toolsBlocked ? (
        <p role="alert" className="notice-inline error linkpage-problem">
          <span className="body">Your account may not give tools to an app. Ask an admin for the permission to manage gateway keys.</span>
        </p>
      ) : (
        <>
          <div role="group" aria-labelledby="oc-sources" className="form-field">
            <p id="oc-sources" className="linkpage-facts-title">Tool sources the app may use</p>
            {req.sources.length === 0 && <p className="help">No tool source is available.</p>}
            {req.sources.map((s) => (
              <label key={s.slug} htmlFor={`oc-src-${s.slug}`} className="checkbox-row">
                <Checkbox
                  id={`oc-src-${s.slug}`}
                  checked={chosen.includes(s.slug)}
                  disabled={busy}
                  onChange={(v) => setSources(v ? [...chosen, s.slug] : chosen.filter((x) => x !== s.slug))}
                />
                <span>{clip(s.name)} <span className="muted mono">{s.slug}</span></span>
              </label>
            ))}
          </div>
          <div className="form-field">
            <label htmlFor="oc-readonly" className="checkbox-row">
              <Switch id="oc-readonly" aria-label="Read-only tools only" checked={ro} disabled={busy} onChange={setReadOnly} />
              <span>Read-only tools only</span>
            </label>
            <p className="help">
              {ro
                ? "The app can use tools that only read. Tools that change something stay out of its reach."
                : "The app can use every tool of the chosen sources, including those that change things."}
            </p>
          </div>
        </>
      ))}

      <p role="note" className="notice-inline warn linkpage-warning">
        <AlertTriangle size={14} className="icon" aria-hidden="true" />
        <span className="body">If you did not just start this connection from an app, deny.</span>
      </p>

      {req.password_required && (
        <FormField
          label="Your password"
          htmlFor="oc-password"
          help="This relay has no separate address for its apps, so connecting one needs your password."
        >
          <Input
            id="oc-password"
            name="password"
            type="password"
            autoComplete="current-password"
            disabled={busy}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </FormField>
      )}

      {problem && <p role="alert" className="notice-inline error linkpage-problem"><span className="body">{problem}</span></p>}

      {/* Two plain buttons, no form: Enter in the fields above decides nothing. */}
      <div className="linkpage-actions">
        <Button type="button" variant="secondary" disabled={busy} onClick={() => decide("deny")}>Deny</Button>
        <Button
          type="button"
          variant="primary"
          disabled={busy || toolsBlocked || noSource || needsPassword}
          onClick={() => decide("approve")}
        >
          Approve
        </Button>
      </div>
    </>
  );
}

/**
 * /oauth/consent?request=<id>: where an authorization request ends up. Shows
 * the app and what it asks for; only the two buttons decide anything, and the
 * page never renders in a foreign frame.
 */
export default function OAuthConsent() {
  const [params] = useSearchParams();
  const id = (params.get("request") ?? "").trim();
  let content: ReactNode;
  if (isFramed()) {
    content = (
      <>
        <h1 className="signin-title">Connect an app</h1>
        <Result kind="alert">Open this page in its own browser tab to continue.</Result>
      </>
    );
  } else if (id === "") {
    content = <Gone />;
  } else {
    content = <Consent key={id} id={id} />;
  }
  return <Shell>{content}</Shell>;
}
