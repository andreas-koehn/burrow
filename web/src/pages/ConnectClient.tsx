import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { clientNameError } from "@/lib/clientName";
import { shellQuote } from "@/lib/shell";
import { Button, FormField, FormFieldGroup, Input, Select, Badge, PageHeader, ErrorNotice } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { useAuth } from "@/auth/useAuth";
import type { NewToken, ClientView } from "@/lib/contract";

interface ConnectInfo { server: string }

const PROTOCOL_OPTIONS = [
  { value: "tcp", label: "TCP" },
  { value: "http", label: "HTTP" },
];

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
  toast.success("Copied.");
}

// Build the CLI command from real field values.
// tcp:  burrow connect --server <ep> --token <tok> --local <local> [--remote <n>] --name <name>
// http: burrow connect --server <ep> --token <tok> --local <local> --type http --name <name>
function buildCmd(opts: {
  endpoint: string;
  token: string;
  local: string;
  remote: string;
  protocol: string;
  name: string;
}): string {
  const { endpoint, token, local, remote, protocol, name } = opts;
  const parts = [
    "burrow connect",
    `--server ${shellQuote(endpoint)}`,
    `--token ${token}`,
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

const BACK = { to: "/clients", label: "Clients" } as const;

export default function ConnectClient() {
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

  // P1-2: control-plane endpoint from relay; fall back to window.location.host
  const ci = useQuery({
    queryKey: ["connect-info"],
    queryFn: () => apiFetch<ConnectInfo>("/clients/connect-info"),
    retry: false,
    staleTime: 5 * 60_000,
  });
  const endpoint = ci.data?.server
    || (typeof window !== "undefined" ? window.location.host : "relay.example.com");

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

  // P2.3 — command built from real fields; masked vs unmasked
  const maskedToken = tok ? "bur_••••••••" : "";
  const realToken = tok?.token ?? "";

  const cmd = tok
    ? buildCmd({ endpoint, token: reveal ? realToken : maskedToken, local, remote, protocol, name })
    : "";
  const cmdToCopy = tok
    ? buildCmd({ endpoint, token: realToken, local, remote, protocol, name })
    : "";

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
    <div className="account-page page-narrow">
      <PageHeader
        back={BACK}
        title="Connect a client"
        subtitle="Bring a machine online so it can expose a local service through this Burrow relay."
      />

      <p className="muted page-intro">
        A client is a machine running <code>burrow connect</code>. Choose what it exposes, name it,
        then run the command on that machine. Already connected?{" "}
        <Link className="link-inline" to="/clients?tab=tokens">Manage tokens</Link>.
      </p>

      {/* P2.2 — What to expose section */}
      <section className="account-section" aria-labelledby="ob-expose">
        <div className="section-head"><div className="left"><h2 id="ob-expose">1. What to expose</h2></div></div>
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
        <div className="section-head"><div className="left"><h2 id="ob-1">2. Name this client</h2></div></div>
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
            <div className="section-head"><div className="left"><h2 id="ob-2" ref={credsRef} tabIndex={-1}>3. Credentials</h2></div></div>
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
                    <Copy size={13} />
                  </button>
                </span>
              </div>
            </div>
          </section>

          <section className="account-section" aria-labelledby="ob-3">
            <div className="section-head"><div className="left"><h2 id="ob-3">4. Run on the client</h2></div></div>
            {/* P2.4 — wrapped command + copy */}
            <div className="row gap-2">
              <pre className="cmd-block wrap flex-1"><code>{cmd}</code></pre>
              <button
                type="button"
                className="icon-btn"
                aria-label="Copy install command"
                onClick={() => copy(cmdToCopy)}
              >
                <Copy size={13} />
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
      <Toaster />
    </div>
  );
}
