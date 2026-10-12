import { shellQuote } from "@/lib/shell";
import type { ClientOs } from "@/lib/platform";

export type { ClientOs };

export interface InstallLineSet {
  /** Downloads and installs the client from the relay itself. */
  install: string;
  /** Signs the machine in: the client opens /link in the browser, where the sign-in is approved. */
  login: string;
  /** Publishes a local port. */
  run: string;
}

export const CLIENT_OS_OPTIONS: readonly { value: ClientOs; label: string }[] = [
  { value: "linux", label: "Linux" },
  { value: "macos", label: "macOS" },
  { value: "windows", label: "Windows" },
];

/** True when the dashboard address is one `burrow login` and the installer accept. */
export function isHttpsOrigin(relayOrigin: string): boolean {
  return /^https:\/\//i.test(relayOrigin);
}

/**
 * Whether a service name can stand in a command as it is. The line is copied
 * into a shell, so nothing else is let through: a name outside these
 * characters is left out, never quoted. It begins with a letter or a digit,
 * so that it is never read as a flag.
 */
export function isCommandName(name: string | undefined): name is string {
  return name !== undefined && /^[a-z0-9][a-z0-9_-]*$/.test(name);
}

/**
 * Whether a value is what `burrow http` publishes: a port, or host:port with
 * a host name, an IPv4 address or an IPv6 address in brackets. Nothing else
 * may reach a command: the value can come from a client's own report, and the
 * line is pasted into a POSIX shell or into PowerShell, which quote differently.
 */
export function isTarget(value: string): boolean {
  const m = /^(?:(?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:.]+\]):)?(\d{1,5})$/.exec(value);
  if (!m) return false;
  const port = Number(m[1]);
  return port >= 1 && port <= 65535;
}

/** What the run line publishes when it is given nothing it can use. */
const EXAMPLE_TARGET = "3000";

/**
 * The three lines that bring a machine online. `relayOrigin` is the address
 * this dashboard is served from (scheme, host, port), which is also where the
 * relay serves its install scripts. An origin that is not https is passed on
 * as it is: the client then says what it needs. A `target` that is not a
 * port or host:port gives way to the example port. With `name` the run line
 * serves the service of that name instead of publishing one of its own; a
 * name that is not safe in a shell is left out.
 */
export function installLines(os: ClientOs, relayOrigin: string, target = EXAMPLE_TARGET, name?: string): InstallLineSet {
  const origin = relayOrigin.replace(/\/+$/, "");
  const relay = isHttpsOrigin(origin) ? origin.slice("https://".length) : origin;
  // Only the brackets of an IPv6 address are left to quote: single quotes are
  // literal in a POSIX shell and in PowerShell alike, and a target holds none.
  const published = shellQuote(isTarget(target) ? target : EXAMPLE_TARGET);
  return {
    install: os === "windows" ? `irm ${origin}/install.ps1 | iex` : `curl -fsSL ${origin}/install.sh | sh`,
    login: `burrow login ${relay}`,
    run: `burrow http ${published}${isCommandName(name) ? ` --name ${name}` : ""}`,
  };
}

export interface DownloadGroup {
  os: ClientOs;
  label: string;
  builds: { arch: string; href: string }[];
}

// The relay's own table (internal/api/client_download.go): a version-tagged
// release has eight builds, the rolling develop channel four.
const RELEASE_BUILDS: Record<ClientOs, string[]> = {
  linux: ["amd64", "arm64", "arm", "386"],
  macos: ["amd64", "arm64"],
  windows: ["amd64", "386"],
};
const DEVELOP_BUILDS: Record<ClientOs, string[]> = {
  linux: ["amd64", "arm64"],
  macos: ["arm64"],
  windows: ["amd64"],
};
const DOWNLOAD_OS: Record<ClientOs, string> = { linux: "linux", macos: "darwin", windows: "windows" };

/** Where the relay hands out its checksum list. */
export const CHECKSUMS_HREF = "/download/burrow/checksums.txt";

/** The client archives a relay of this version redirects to, by operating system. */
export function downloadTargets(relayVersion: string): DownloadGroup[] {
  const builds = /^v?\d{1,9}\.\d{1,9}\.\d{1,9}$/.test(relayVersion.trim()) ? RELEASE_BUILDS : DEVELOP_BUILDS;
  return CLIENT_OS_OPTIONS.map(({ value, label }) => ({
    os: value,
    label,
    builds: builds[value].map((arch) => ({ arch, href: `/download/burrow/${DOWNLOAD_OS[value]}/${arch}` })),
  }));
}
