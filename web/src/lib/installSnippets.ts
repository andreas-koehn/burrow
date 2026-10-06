import { shellQuote } from "@/lib/shell";
import type { ClientOs } from "@/lib/platform";

export type { ClientOs };

export interface InstallLineSet {
  /** Downloads and installs the client from the relay itself. */
  install: string;
  /** Signs the machine in. The token is read from standard input, never from the command line. */
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
 * The three lines that bring a machine online. `relayOrigin` is the address
 * this dashboard is served from (scheme, host, port), which is also where the
 * relay serves its install scripts. An origin that is not https is passed on
 * as it is: the client then says what it needs.
 */
export function installLines(os: ClientOs, relayOrigin: string, target = "3000"): InstallLineSet {
  const origin = relayOrigin.replace(/\/+$/, "");
  const relay = isHttpsOrigin(origin) ? origin.slice("https://".length) : origin;
  return {
    install: os === "windows" ? `irm ${origin}/install.ps1 | iex` : `curl -fsSL ${origin}/install.sh | sh`,
    // Browser sign-in is not there yet: the client asks for a token and reads it from standard input.
    login: `burrow login ${relay} --token -`,
    run: `burrow http ${shellQuote(target)}`,
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
