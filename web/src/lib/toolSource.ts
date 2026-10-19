import type { ToolSource, ToolSourceTool } from "./contract";

const SLUG = /^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$/;
const RESERVED = ["burrow", "v1"];

/** The relay's rule for the slug of a tool source (D11), or null. */
export function toolSlugError(slug: string): string | null {
  if (slug === "") return "Enter a name.";
  if (RESERVED.includes(slug)) return `"${slug}" is reserved. Choose another name.`;
  if (!SLUG.test(slug)) {
    return "Use 1 to 24 letters, digits or hyphens, starting and ending with a letter or digit.";
  }
  return null;
}

/** The slug the relay would suggest for a name: lower case, runs of other characters one hyphen, 24 characters. */
export function toolSlugFromName(name: string): string {
  let slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  if (slug.length > 24) slug = slug.slice(0, 24).replace(/-+$/, "");
  return slug;
}

/** A rename of a tool: A-Z a-z 0-9 _ - such that `<slug>__<rename>` is at most 64 characters. "" clears it. */
export function toolRenameError(slug: string, rename: string): string | null {
  if (rename === "") return null;
  if (!/^[A-Za-z0-9_-]+$/.test(rename)) return "Use letters, digits, _ and - only.";
  const room = 64 - slug.length - 2;
  if (rename.length > room) return `Too long: at most ${room} characters here, so that ${slug}__<name> stays within 64.`;
  return null;
}

export const MAX_DESCRIPTION_OVERRIDE_BYTES = 8192;

/** The state of a source as one phrase, for the list and the page. */
export function sourceStatus(s: Pick<ToolSource, "enabled" | "last_sync_at" | "last_sync_error" | "pending_count">):
  { label: string; kind: "status-online" | "status-never" | "status-idle" | "status-suspended" } {
  if (!s.enabled) return { label: "Disabled", kind: "status-never" };
  if (s.last_sync_error) return { label: "Sync failed", kind: "status-suspended" };
  if (!s.last_sync_at) return { label: "Not synced yet", kind: "status-never" };
  if (s.pending_count > 0) return { label: `${s.pending_count} to approve`, kind: "status-idle" };
  return { label: "Synced", kind: "status-online" };
}

/** What a tool is, as one word for the catalog. */
export function toolStatus(t: ToolSourceTool): { label: string; kind: string } {
  if (t.status === "removed") return { label: "Removed", kind: "status-never" };
  if (t.status === "pending") return { label: t.approved_hash === "" ? "New" : "Changed", kind: "status-idle" };
  if (t.hidden) return { label: "Hidden", kind: "status-never" };
  if (t.serve_issue === "name_conflict") return { label: "Name conflict", kind: "status-suspended" };
  return { label: "Served", kind: "status-online" };
}

/** Splits a command line the way a shell does for plain quotes: spaces separate, quotes group. */
export function splitCommand(line: string): string[] {
  const out: string[] = [];
  let cur = "";
  let quote: string | null = null;
  let has = false;
  for (const ch of line.trim()) {
    if (quote) {
      if (ch === quote) quote = null;
      else cur += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch;
      has = true;
    } else if (/\s/.test(ch)) {
      if (has || cur !== "") out.push(cur);
      cur = "";
      has = false;
    } else {
      cur += ch;
    }
  }
  if (has || cur !== "") out.push(cur);
  return out;
}

const SAFE_ARG = /^[A-Za-z0-9_@%+=:,./-]+$/;
function shellWord(w: string): string {
  return SAFE_ARG.test(w) ? w : `'${w.replace(/'/g, `'\\''`)}'`;
}

/** Whether the text is the address of a server that already listens on the machine. */
export function isLocalUrl(text: string): boolean {
  return /^https?:\/\/\S+$/.test(text.trim());
}

/** The `burrow mcp` command for a source (client: `burrow mcp <name> -- <command>` or `burrow mcp <name> <url>`). */
export function burrowMcpCommand(slug: string, commandOrUrl: string): string {
  const text = commandOrUrl.trim();
  if (isLocalUrl(text)) return `burrow mcp ${slug} ${text}`;
  return `burrow mcp ${slug} -- ${splitCommand(text).map(shellWord).join(" ")}`;
}

const yamlStr = (s: string) => JSON.stringify(s);

/** The same server as a `burrow.yaml` service (type: mcp: exactly one of command and url). */
export function burrowYamlSnippet(slug: string, commandOrUrl: string): string {
  const text = commandOrUrl.trim();
  const head = `services:\n  - name: ${slug}\n    type: mcp\n`;
  if (isLocalUrl(text)) return `${head}    url: ${yamlStr(text)}\n`;
  const [command = "", ...args] = splitCommand(text);
  const argsLine = args.length > 0 ? `    args: [${args.map(yamlStr).join(", ")}]\n` : "";
  return `${head}    command: ${yamlStr(command)}\n${argsLine}`;
}
