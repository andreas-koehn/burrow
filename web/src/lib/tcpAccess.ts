import type { AccessMode, TcpAccessMode } from "@/lib/contract";

export const ACCESS_LABEL: Record<AccessMode, string> = {
  open: "Open",
  api_key: "API key",
  burrow_login: "Burrow login",
  mtls: "mTLS",
  ip: "IP allowlist",
  private: "Private",
};

/** The local port `burrow forward` listens on unless told otherwise. */
export const FORWARD_LOCAL_PORT = 13389;

/** Ports whose services must not meet the internet unguarded, with what listens there. */
export const RISKY_PORTS: Record<number, string> = {
  3389: "Remote Desktop",
  22: "SSH",
  5900: "VNC",
  5432: "Postgres",
};

/** The port of a `host:port` address, or null when there is none. */
export function localPort(addr: string): number | null {
  const m = /:(\d{1,5})$/.exec(addr.trim());
  return m ? Number(m[1]) : null;
}

/** The port to warn about when the service listens on one that is risky to open. */
export function riskyPort(addr: string): number | null {
  const p = localPort(addr);
  return p !== null && p in RISKY_PORTS ? p : null;
}

export function forwardCommand(target: string): string {
  return `burrow forward ${target} ${FORWARD_LOCAL_PORT}`;
}

/** The range one step wider than an address: its /24 (IPv4) or /64 (IPv6). Null when there is no wider step. */
export function widenCidr(cidr: string): string | null {
  const [addr, bitsText] = cidr.split("/");
  const bits = bitsText === undefined ? null : Number(bitsText);
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(addr)) {
    if (bits !== null && bits <= 24) return null;
    return `${addr.split(".").slice(0, 3).join(".")}.0/24`;
  }
  if (addr.includes(":") && /^[0-9a-f:]+$/i.test(addr)) {
    if (bits !== null && bits <= 64) return null;
    const [head, tail = ""] = addr.split("::");
    const h = head ? head.split(":") : [];
    const t = tail ? tail.split(":") : [];
    const groups = addr.includes("::") ? [...h, ...Array(Math.max(0, 8 - h.length - t.length)).fill("0"), ...t] : h;
    if (groups.length !== 8) return null;
    const net = groups.slice(0, 4).map((g) => g.replace(/^0+(?=.)/, "").toLowerCase());
    // Trailing zero groups collapse into "::", the form the relay writes.
    let end = net.length;
    while (end > 0 && net[end - 1] === "0") end--;
    if (end === net.length) return `${net.join(":")}::/64`;
    return `${net.slice(0, end).join(":")}::/64`;
  }
  return null;
}

/** How long a grant has left, measured against `now`. */
export function remaining(expiresAt: string, now: number = Date.now()): string {
  const ms = Date.parse(expiresAt) - now;
  if (!(ms > 0)) return "expired";
  const min = Math.floor(ms / 60_000);
  if (min < 1) return "less than a minute left";
  if (min < 60) return `${min} min left`;
  const h = Math.floor(min / 60);
  if (h < 24) return min % 60 ? `${h} h ${min % 60} min left` : `${h} h left`;
  return `${Math.floor(h / 24)} d left`;
}

const RESTRICTIVENESS: Partial<Record<AccessMode, number>> = { open: 0, ip: 1, private: 2 };

/** Higher is stricter: a change to a higher value narrows who can reach the service. */
export function restrictiveness(mode: AccessMode): number {
  return RESTRICTIVENESS[mode] ?? 0;
}

function isIPv4(s: string): boolean {
  const parts = s.split(".");
  return parts.length === 4 && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
}

function isIPv6(s: string): boolean {
  if (!s.includes(":") || s.split("::").length > 2) return false;
  // An IPv4 tail ("::ffff:1.2.3.4") counts as two groups.
  const last = s.slice(s.lastIndexOf(":") + 1);
  let head = s;
  if (last.includes(".")) {
    if (!isIPv4(last)) return false;
    head = s.slice(0, s.lastIndexOf(":") + 1) + "0:0";
  }
  const compressed = head.includes("::");
  const groups = head.split(/::|:/).filter((g, i, a) => !(g === "" && (compressed || i === a.length - 1)));
  if (!groups.every((g) => /^[0-9a-f]{1,4}$/i.test(g))) return false;
  const n = groups.length;
  return compressed ? n < 8 : n === 8;
}

/** An address or a range as the relay takes it: IPv4 or IPv6, with an optional prefix length. */
export function isAddrOrCidr(text: string): boolean {
  const parts = text.trim().split("/");
  if (parts.length > 2) return false;
  const [addr, bits] = parts;
  let max: number;
  if (isIPv4(addr)) max = 32;
  else if (isIPv6(addr)) max = 128;
  else return false;
  if (bits === undefined) return true;
  return /^\d{1,3}$/.test(bits) && Number(bits) <= max;
}

interface TunnelRef { name: string; type: string; service_id?: string }
interface ServiceRef { id: string; name: string; type: string }

/**
 * The tcp service a live tunnel serves. A tunnel that names its service id is
 * taken at its word; otherwise the name decides, and only when exactly one tcp
 * service has it: names are per owner, so two services may share one.
 */
export function matchTcpService<S extends ServiceRef>(services: readonly S[], tunnel: TunnelRef): S | undefined {
  if (tunnel.service_id) return services.find((s) => s.id === tunnel.service_id);
  if (tunnel.type !== "tcp") return undefined;
  const named = services.filter((s) => s.type === "tcp" && s.name === tunnel.name);
  return named.length === 1 ? named[0] : undefined;
}

/** What the relay's first-bytes classifier calls a connection, as it is shown. */
export const PROTOCOL_LABEL: Record<string, string> = {
  tls: "TLS",
  ssh: "SSH",
  rdp: "RDP",
  http: "HTTP",
  unknown: "Unknown",
};

/** The text of a protocol label; "" while none was recognised (or the relay predates it). */
export function protocolLabel(protocol: string | undefined): string {
  return protocol ? PROTOCOL_LABEL[protocol] ?? protocol : "";
}

/** The connection-log reasons of a tcp connection that ended or was refused by a rule, in words. */
export const TCP_REASON: Record<string, string> = {
  banned: "banned source",
  quota: "daily volume reached",
  idle_timeout: "idle timeout",
  proxy_unsupported: "client cannot send the PROXY header",
};

/** A reason as the log shows it: a known one in words, any other as the relay wrote it. */
export function reasonText(reason: string): string {
  return TCP_REASON[reason] ?? reason;
}

/** The ISO country code a reader typed, or null when it is not two letters. */
export function countryCode(text: string): string | null {
  const c = text.trim().toUpperCase();
  return /^[A-Z]{2}$/.test(c) ? c : null;
}

/** The three access modes of a tcp service, as the pages offer them. */
export const MODES: { value: TcpAccessMode; title: string; help: string }[] = [
  { value: "open", title: "Open", help: "A public port that anyone on the internet can connect to. Burrow adds no authentication." },
  { value: "ip", title: "IP allowlist", help: "A public port that only the addresses you list, or allow for a few hours, can connect to. Everyone else is closed out before your machine hears of it." },
  { value: "private", title: "Private", help: "No public port. Reachable only through burrow forward, by you and the roles you allow." },
];

/** `burrow tcp` for a service created in the dashboard: it finds the service by its name and serves it in the mode stored. */
export function tcpClientCommand(name: string, localPort: string): string {
  return `burrow tcp ${localPort.trim() || "<port>"} --name ${name}`;
}

/** The same service as an entry of burrow.yaml (`burrow up`). */
export function tcpClientYaml(name: string, localPort: string): string {
  return `services:\n  - name: ${name}\n    type: tcp\n    local: 127.0.0.1:${localPort.trim() || "<port>"}`;
}
