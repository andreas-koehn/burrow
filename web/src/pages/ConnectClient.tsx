import { useEffect, useId, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, CircleCheck, Copy } from "lucide-react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { clientNameError } from "@/lib/clientName";
import { shellQuote } from "@/lib/shell";
import { Button, FormField, FormFieldGroup, Input, Select, Badge, PageHeader, ErrorNotice } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { useAuth } from "@/auth/useAuth";
import { InstallLines } from "@/components/InstallLines";
import { CHECKSUMS_HREF, downloadTargets } from "@/lib/installSnippets";
import type { NewToken, ClientView, ClientDiscovery } from "@/lib/contract";

interface ConnectInfo { server: string }

const PROTOCOL_OPTIONS = [
  { value: "tcp", label: "TCP" },
  { value: "http", label: "HTTP" },
];

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
  toast.success("Copied.");
}

// Build the CLI command from real field values. The token comes from the
// shell variable BURROW_TOKEN (the same spelling works in a POSIX shell and in
// PowerShell); the page never puts a token into a command line.
// tcp:  burrow connect --server <ep> --token "$BURROW_TOKEN" --local <local> [--remote <n>] --name <name>
// http: burrow connect --server <ep> --token "$BURROW_TOKEN" --local <local> --type http --name <name>
function buildCmd(opts: {
  endpoint: string;
  local: string;
  remote: string;
  protocol: string;
  name: string;
}): string {
  const { endpoint, local, remote, protocol, name } = opts;
  const parts = [
    "burrow connect",
    `--server ${shellQuote(endpoint)}`,
    `--token "$BURROW_TOKEN"`,
    `--local ${shellQuote(local)}`,
  ];
  if (protocol === "tcp" && remote.trim() !== "") {
    parts.push(`--remote ${shellQuote(remote.trim())}`);
  }
  if (protocol === "http") {
    parts.push("--type http");
  }
  parts.push(`--name ${shellQuote(name)}`);
  return parts.join(" ");
}

/**
 * The form this page used to be: choose what to expose, name the client, get
 * a freshly minted token and a `burrow connect` command. The command takes the
 * token from a shell variable, so no command line the page shows or copies
 * ever holds it.
 */
function ConnectForm({ endpoint }: { endpoint: string }) {
  const [name, setName] = useState("");
  const [reveal, setReveal] = useState(false);
  const [error, setError] = useState("");
  // P2.2 — expose fields
  const [local, setLocal] = useState("127.0.0.1:3000");
  const [remote, setRemote] = useState("");
  const [protocol, setProtocol] = useState("tcp");
  const [nameError, setNameError] = useState<string | null>(null);
  const credsRef = useRef<HTMLHeadingElement>(null);
  const nameRef = useRef<HTMLInputElement>(null);
  const restartedRef = useRef(false);

  const { user } = useAuth();
  const isAdmin = user?.role === "admin";

  const mint = useMutation({
    mutationFn: () => apiFetch<NewToken>("/tokens", { method: "POST", body: JSON.stringify({ name }) }),
    onError: (e: unknown) => setError(e instanceof ApiError ? e.message : "Failed to mint token"),
  });

  const tok = mint.data;

  // Bring the result into view: it renders below the form and was easy to miss.
  useEffect(() => {
    if (!tok) return;
    credsRef.current?.scrollIntoView?.({ behavior: "smooth", block: "start" });
    // preventScroll: a plain focus() jumps instantly and cancels the smooth scroll.
    credsRef.current?.focus({ preventScroll: true });
  }, [tok]);

  // Starting over unmounts the focused button; hand focus to the name field
  // once it is enabled again (it is still disabled during the click).
  useEffect(() => {
    if (tok || !restartedRef.current) return;
    restartedRef.current = false;
    nameRef.current?.focus();
  }, [tok]);

  function generate() {
    const problem = clientNameError(name);
    setNameError(problem);
    if (problem) return;
    setError("");
    mint.mutate();
  }

  function startOver() {
    restartedRef.current = true;
    mint.reset();
    setName("");
    setReveal(false);
    setNameError(null);
    setError("");
  }

  // P2.3 — command built from real fields. The token is not one of them.
  const cmd = tok ? buildCmd({ endpoint, local, remote, protocol, name }) : "";

  // P2.5 — success-loop poller (admin only)
  const { data: clientsData } = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    enabled: !!tok && isAdmin,
    refetchInterval: (query) => {
      const d = query.state.data as ClientView[] | undefined;
      const alreadyConnected = d?.some((c) => c.token_name === name) ?? false;
      return alreadyConnected ? false : 3000;
    },
    retry: false,
  });

  const matched = clientsData?.find((c) => c.token_name === name);
  const connected = !!matched;

  return (
    <>
      {/* P2.2 — What to expose section */}
      <section className="account-section" aria-labelledby="ob-expose">
        <div className="section-head"><div className="left"><h3 id="ob-expose">1. What to expose</h3></div></div>
        <FormFieldGroup>
          <FormField
            label="Local address"
            htmlFor="ob-local"
            w="md"
            help="host:port of the app on the client machine"
          >
            <Input
              id="ob-local"
              aria-label="Local address"
              value={local}
              onChange={(e) => setLocal(e.target.value)}
              placeholder="127.0.0.1:3000"
            />
          </FormField>
          <FormField
            label="Public port"
            htmlFor="ob-remote"
            w="sm"
            help="requested public port — leave blank for auto; ignored for HTTP"
          >
            <Input
              id="ob-remote"
              aria-label="Public port"
              type="number"
              value={remote}
              onChange={(e) => setRemote(e.target.value)}
              placeholder="auto"
              disabled={protocol === "http"}
            />
          </FormField>
          <FormField
            label="Protocol"
            htmlFor="ob-protocol"
            w="sm"
            help="TCP for raw ports, HTTP for web apps"
          >
            <Select
              id="ob-protocol"
              options={PROTOCOL_OPTIONS}
              value={protocol}
              onChange={(v) => {
                setProtocol(v);
                if (v === "http") setRemote("");
              }}
            />
          </FormField>
        </FormFieldGroup>
      </section>

      <section className="account-section" aria-labelledby="ob-1">
        <div className="section-head"><div className="left"><h3 id="ob-1">2. Name this client</h3></div></div>
        <FormFieldGroup>
          <FormField
            label="Client name"
            htmlFor="ob-name"
            w="md"
            help="Lowercase letters, digits, and hyphens. Once issued, the name lives with the token."
            error={nameError ?? undefined}
          >
            <Input
              id="ob-name"
              ref={nameRef}
              aria-label="Client name"
              aria-invalid={nameError ? true : undefined}
              value={name}
              disabled={!!tok}
              onChange={(e) => { setName(e.target.value); if (nameError) setNameError(null); }}
              placeholder="e.g. office-box-1"
            />
          </FormField>
        </FormFieldGroup>
        <div className="actions">
          {tok ? (
            <Button variant="secondary" size="sm" onClick={startOver}>Connect another client</Button>
          ) : (
            <Button variant="primary" size="sm" disabled={mint.isPending} onClick={generate}>
              {mint.isPending ? "Generating…" : "Generate token"}
            </Button>
          )}
        </div>
        {error && <p role="alert" className="field-error">{error}</p>}
      </section>

      {tok && (
        <>
          <section className="account-section" aria-labelledby="ob-2">
            <div className="section-head"><div className="left"><h3 id="ob-2" ref={credsRef} tabIndex={-1}>3. Credentials</h3></div></div>
            <ErrorNotice variant="warn" role="status">
              Store this token now. Burrow doesn't keep a copy you can retrieve later — if you lose it, mint a new one for this client.
            </ErrorNotice>
            <div className="stack-md">
              <div className="field">
                <label>Server endpoint</label>
                {/* P2.6 — inline endpoint explainer */}
                <span className="row row-center gap-2">
                  <code className="mono">{endpoint}</code>
                  <span className="muted" style={{ fontSize: "0.8em" }}>
                    This is the relay's reachable address — a client on another machine must be able to reach it.
                  </span>
                </span>
              </div>
              <div className="field">
                <label>Client token</label>
                <span className="row row-center gap-2">
                  <code className="mono">{reveal ? tok.token : "bur_••••••••"}</code>
                  <Button variant="ghost" size="sm" aria-label={reveal ? "Hide token" : "Reveal token"} onClick={() => setReveal((r) => !r)}>
                    {reveal ? "Hide" : "Reveal"}
                  </Button>
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label="Copy client token"
                    onClick={() => copy(tok.token)}
                  >
                    <Copy size={13} aria-hidden="true" />
                  </button>
                </span>
              </div>
            </div>
          </section>

          <section className="account-section" aria-labelledby="ob-3">
            <div className="section-head"><div className="left"><h3 id="ob-3">4. Run on the client</h3></div></div>
            <p className="muted small">
              The command takes the token from the shell variable <code>BURROW_TOKEN</code>, so the token
              stays out of the command line you copy and out of the shell history. Set it first: run{" "}
              <code>read -rs BURROW_TOKEN</code> (in PowerShell <code>$BURROW_TOKEN = Read-Host</code>),
              paste the token and press Enter.
            </p>
            {/* P2.4 — wrapped command + copy */}
            <div className="row gap-2">
              <pre id="connect-command" className="cmd-block wrap fill-rest"><code>{cmd}</code></pre>
              <button
                type="button"
                className="icon-btn"
                aria-label="Copy connect command"
                onClick={() => copy(cmd)}
              >
                <Copy size={13} aria-hidden="true" />
              </button>
            </div>

            {/* P2.5 — success loop status region */}
            <div role="status" style={{ marginTop: "var(--space-4)" }}>
              {isAdmin ? (
                connected && matched ? (
                  <span className="row row-center gap-2">
                    <Badge kind="status-connected">connected</Badge>
                    <span>✓ <strong>{name}</strong> connected</span>
                    <Link to={`/clients/${matched.session_id}`}>View client</Link>
                  </span>
                ) : (
                  <span className="muted">Waiting for <strong>{name}</strong> to connect…</span>
                )
              ) : (
                <span className="muted">Your client will appear under Clients once it connects.</span>
              )}
            </div>
          </section>
        </>
      )}
    </>
  );
}

const BACK = { to: "/clients", label: "Clients" } as const;

function yamlExample(endpoint: string): string {
  return [
    `server: ${endpoint}`,
    "token_file: /etc/burrow/token",
    "services:",
    "  - name: web",
    "    local: 127.0.0.1:3000",
    "    type: http",
  ].join("\n");
}

/**
 * The page's live state below the three lines: it asks for the clients list
 * every two seconds and reports the first client that was not there when the
 * page opened. Only an admin can list clients; anyone else is told where to look.
 */
function WaitForClient() {
  const { user, loading } = useAuth();
  const isAdmin = user?.role === "admin";
  // The clients that were connected already, from the first answer after the page opened.
  const [known, setKnown] = useState<ReadonlySet<string> | null>(null);
  const [arrived, setArrived] = useState<ClientView | null>(null);

  const clients = useQuery({
    queryKey: ["clients"],
    queryFn: () => apiFetch<ClientView[]>("/clients"),
    // Once a client is here there is nothing left to ask.
    enabled: isAdmin && !arrived,
    refetchInterval: (query) => (query.state.status === "error" ? false : 2000),
    retry: false,
  });

  // An answer from before this page opened (the Clients page shares the query) says nothing about now.
  if (isAdmin && !arrived && clients.isFetchedAfterMount && Array.isArray(clients.data)) {
    if (known === null) {
      setKnown(new Set(clients.data.map((c) => c.session_id)));
    } else {
      const fresh = clients.data.find((c) => !known.has(c.session_id));
      if (fresh) setArrived(fresh);
    }
  }

  if (loading) return null;
  return (
    <div role="status" className="connect-wait">
      {arrived ? (
        <>
          <CircleCheck size={16} aria-hidden="true" className="connect-wait-done" />
          <span>
            Connected: <Link className="link-inline" to={`/clients/${arrived.session_id}`}>{arrived.token_name}</Link>
          </span>
        </>
      ) : isAdmin && !clients.isError ? (
        <>
          <span className="spinner" aria-hidden="true" />
          <span className="muted">Waiting for your client…</span>
        </>
      ) : (
        <span className="muted">Your client appears under Clients once it connects.</span>
      )}
    </div>
  );
}

export default function ConnectClient() {
  const otherId = useId();
  // What the reader chose; until then the section follows the relay's age.
  const [otherChoice, setOtherChoice] = useState<boolean | null>(null);

  // P1-2: control-plane endpoint from relay; fall back to window.location.host
  const ci = useQuery({
    queryKey: ["connect-info"],
    queryFn: () => apiFetch<ConnectInfo>("/clients/connect-info"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  // Tells the relay's version, and by its absence that the relay predates the three lines.
  const discovery = useQuery({
    queryKey: ["client-discovery"],
    queryFn: () => apiFetch<ClientDiscovery>("/client/discovery"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  const olderRelay = discovery.error instanceof ApiError && discovery.error.status === 404;
  const otherOpen = otherChoice ?? olderRelay;

  const origin = typeof window !== "undefined" ? window.location.origin : "";
  const endpoint = ci.data?.server
    || discovery.data?.control
    || (typeof window !== "undefined" ? window.location.host : "relay.example.com");
  const downloads = discovery.data ? downloadTargets(discovery.data.version) : null;

  return (
    <div className="account-page page-narrow">
      <PageHeader
        back={BACK}
        title="Connect a client"
        subtitle="Bring a machine online so it can expose a local service through this Burrow relay."
      />

      <p className="muted page-intro">
        A client is a machine running <code>burrow</code>. Run these three lines on it: they install the
        client from this relay, sign the machine in and publish a local port. Already connected?{" "}
        <Link className="link-inline" to="/clients?tab=tokens">Manage tokens</Link>.
      </p>

      {olderRelay && (
        <ErrorNotice variant="warn" role="status">
          This relay is older than the client commands below; use 'Other ways to connect'.
        </ErrorNotice>
      )}

      <InstallLines relayOrigin={origin} />
      <WaitForClient />

      <section className="account-section" aria-labelledby={`${otherId}-head`}>
        <h2 id={`${otherId}-head`} className="connect-other-head">
          <Button
            variant="ghost"
            size="sm"
            aria-expanded={otherOpen}
            aria-controls={`${otherId}-panel`}
            icon={otherOpen ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}
            onClick={() => setOtherChoice(!otherOpen)}
          >
            Other ways to connect
          </Button>
        </h2>
        <div id={`${otherId}-panel`} hidden={!otherOpen}>
          {otherOpen && (
            <>
              <section className="account-section" aria-labelledby="ob-download">
                <div className="section-head"><div className="left"><h3 id="ob-download">Download by hand</h3></div></div>
                {downloads ? (
                  <>
                    <p className="muted small">
                      The same archives the installer fetches. Unpack one and put <code>burrow</code> on your <code>PATH</code>.
                    </p>
                    <ul role="list" className="connect-downloads">
                      {downloads.map((g) => (
                        <li key={g.os}>
                          <span className="connect-downloads-os">{g.label}</span>
                          {g.builds.map((b) => (
                            // A plain link: the relay answers this path, not the dashboard's router.
                            <a key={b.arch} className="link-inline" href={b.href} aria-label={`${g.label} ${b.arch}`}>{b.arch}</a>
                          ))}
                        </li>
                      ))}
                    </ul>
                    <p className="muted small">
                      Compare the archive's SHA-256 with <a className="link-inline" href={CHECKSUMS_HREF}>checksums.txt</a>{" "}
                      to see that the download is complete and undamaged.
                    </p>
                  </>
                ) : olderRelay ? (
                  <p className="muted small">
                    This relay does not hand out the client. Take the <code>burrow</code> archive from the same release as the relay.
                  </p>
                ) : (
                  <p className="muted small">Asking the relay which builds it offers…</p>
                )}
              </section>

              <ConnectForm endpoint={endpoint} />

              <section className="account-section" aria-labelledby="ob-yaml">
                <div className="section-head"><div className="left"><h3 id="ob-yaml">burrow.yaml</h3></div></div>
                <p className="muted small">
                  Several services, kept in a file. The token lives in a file of its own that only you can read.
                  Start it with <code>burrow connect --config burrow.yaml</code>.
                </p>
                <pre className="cmd-block"><code>{yamlExample(endpoint)}</code></pre>
              </section>
            </>
          )}
        </div>
      </section>
      <Toaster />
    </div>
  );
}
