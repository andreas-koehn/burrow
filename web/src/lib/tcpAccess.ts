import type { AccessMode } from "@/lib/contract";

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
