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
