import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

// Words that may not appear in what the user reads. Checked in string literals
// and JSX text only: comments and import lines are stripped first, so a
// component named ClientTokensPanel or a type ClientView does not count.
// Every rule ignores case and is matched with whitespace runs collapsed, so a
// phrase wrapped over two JSX lines counts as well.
const RETIRED: [RegExp, string][] = [
  [/tunnell?ing agents?/i, "say client"],
  [/\bAI services?\b/i, "say provider"],
  [/\bAI endpoints?\b|across endpoints\b/i, "say provider"],
  [/\bclient offline\b/i, "say Offline"],
  // As JSX text, or as a literal of its own ({x ? "connected" : "idle"}). A
  // value that only contains the word (closed_idle, the badge kind status-idle) is not a status label.
  [/>\s*idle\s*<|["'`]idle["'`]/i, "say Offline or Never connected"],
  // A client is a machine running burrow: it signs in, sends its name and asks
  // its local server, so each alternative names what only an app does: asking
  // for a model, sending one, using a URL or a key.
  [/\bclients? (can )?asks? for\b|\bclients? sends? as the model\b|\bclients that (ask for|use (it|this (base )?URL))\b|\bmodels for other clients\b|\bclients using this key\b/i, "say app"],
  [/OpenAI-compatible client|Anthropic-compatible client|\b(use|paste|set) (it |this )?in a client\b/i, "say app"],
  [/Rotate cache/i, "removed"],
  [/edit this table in Settings/i, "no such page"],
  // One variable for the key an app sends to the AI gateway.
  [/BURROW_API_KEY/i, "say BURROW_GATEWAY_KEY"],
];

/** The retired wordings in a source text, each with what to say instead. */
function retiredIn(src: string): string[] {
  const text = readable(src).replace(/\s+/g, " ");
  return RETIRED.flatMap(([re, instead]) => {
    const m = text.match(re);
    return m ? [`"${m[0]}": ${instead}`] : [];
  });
}

// Mocks and tests quote the relay and the old words; the contract mirrors the wire.
const ALLOW = [/mocks\//, /\.test\.tsx?$/, /contract\.ts$/, /vocabulary\.test\.ts$/];

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

/**
 * The source without what the user never reads: comments and import lines.
 * Strings are kept and never looked into for comments, so the "//" of a URL
 * and the "/*" of a glob stay; where the scan is unsure it keeps the text, so
 * a mistake shows as a failure, not as a word that slipped through.
 */
function readable(src: string): string {
  let out = "";
  let quote = ""; // the delimiter of the string the scan is in
  for (let i = 0; i < src.length; i++) {
    const c = src[i]!;
    const next = src[i + 1] ?? "";
    if (quote) {
      out += c;
      if (c === "\\") { out += next; i++; }
      // Only a template literal runs over a line end (an apostrophe in JSX text does not open one).
      else if (c === quote || (c === "\n" && quote !== "`")) quote = "";
      continue;
    }
    if (c === "\\") { out += c + next; i++; continue; } // an escaped slash in a regex literal
    if (c === "/" && next === "*") {
      const end = src.indexOf("*/", i + 2);
      i = end < 0 ? src.length : end + 1;
      continue;
    }
    if (c === "/" && next === "/") {
      const end = src.indexOf("\n", i);
      i = end < 0 ? src.length : end - 1;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") quote = c;
    out += c;
  }
  return out
    .replace(/\{\s*\}/g, "") // what a {/* JSX comment */} leaves behind
    .replace(/^\s*import\s+(?:type\s+)?[\w*{][^;]*?\bfrom\s+["'][^"']+["'];?/gm, "")
    .replace(/^\s*import\s+["'][^"']+["'];?/gm, "");
}

describe("readable", () => {
  it("drops comments and import lines and keeps what is shown", () => {
    const src = [
      'import { Idle } from "./AI service";',
      "import {",
      "  Thing,",
      '} from "./across endpoints";',
      "// an AI service in a line comment",
      "/* Rotate cache in a block comment */",
      "interface Props {",
      "  /** client offline, in a doc comment right after a brace */",
      "  a: string;",
      "}",
      "const a = <p>{/* client offline */}Shown</p>;",
      'const url = "https://example.com/x"; // tunneling agent',
      'const glob = "/api/*"; const kept = "after the glob"; /* Rotate cache */',
    ].join("\n");
    const out = readable(src);
    expect(out).toContain("<p>Shown</p>");
    expect(out).toContain('"https://example.com/x"');
    expect(out).toContain('"after the glob"');
    expect(out).toContain("a: string;");
    for (const [re] of RETIRED) expect(out).not.toMatch(re);
  });

  it("keeps string literals and JSX text, where the retired words would be read", () => {
    const out = readable([
      'const label = "Global (cache shared across endpoints)";',
      "const b = <Badge>idle</Badge>;",
      "const c = <p>It isn't there. Rotate cache</p>;",
    ].join("\n"));
    expect(out).toMatch(/shared across endpoints/);
    expect(out).toMatch(/>\s*idle\s*</);
    expect(out).toMatch(/Rotate cache/);
  });
});

describe("the guard itself", () => {
  // Each retired wording as it was written, wrapped over two lines, and in another case.
  const CAUGHT: [string, string][] = [
    ["tunneling agents", "<p>not tunneling agents.</p>"],
    ["tunneling agents, wrapped", "<p>\n  not tunneling\n  agents.\n</p>"],
    ["tunneling agents, other case", "<p>Tunnelling Agent</p>"],
    ["AI service", "<Button>New AI service</Button>"],
    ["AI service, wrapped", "<Button>\n  New AI\n  service\n</Button>"],
    ["AI service, other case", 'const title = "New AI Service";'],
    ["AI endpoint", 'const t = "One AI endpoint";'],
    ["AI endpoint, wrapped", "<p>Every AI\n  endpoint</p>"],
    ["AI endpoint, other case", 'const t = "ai endpoints";'],
    ["across endpoints", 'const l = "Global (cache shared across endpoints)";'],
    ["across endpoints, wrapped", "<p>shared across\n  endpoints</p>"],
    ["across endpoints, other case", 'const l = "Across Endpoints";'],
    ["client offline", "<Badge>client offline</Badge>"],
    ["client offline, wrapped", "<Badge>\n  client\n  offline\n</Badge>"],
    ["client offline, other case", 'const t = "Client Offline";'],
    ["idle", "<Badge>idle</Badge>"],
    ["idle, wrapped", "<Badge>\n  idle\n</Badge>"],
    ["idle, other case", "<Badge>Idle</Badge>"],
    ["idle as a literal", '<Badge>{up ? "connected" : "idle"}</Badge>'],
    ["idle as a literal, other case", "const label = up ? 'Online' : 'Idle';"],
    ["clients ask for", 'const s = "Names clients ask for, and where each one is sent.";'],
    ["clients ask for, wrapped", "<p>A model is a name clients\n  ask for.</p>"],
    ["clients ask for, other case", "<p>Clients Ask for it</p>"],
    ["a client asks for", "<p>What a client asks for when you add a provider by hand.</p>"],
    ["clients can ask for", "<p>give it a name clients can ask for.</p>"],
    ["clients send", 'const h = "What clients send as the model";'],
    ["clients send, wrapped", "<p>A name clients\n  send as the model, and where each API format sends it.</p>"],
    ["a client sends as the model", "<p>the name a client Sends As The Model</p>"],
    ["clients that ask for", "<p>Clients that ask for this model will get an error.</p>"],
    ["clients that use this base URL", "<p>Clients that\n  use this base URL will start receiving errors.</p>"],
    ["clients that use this URL", "<p>clients that use this url</p>"],
    ["clients that use it", "<p>The old URL stops working immediately. Clients that use it must be updated.</p>"],
    ["models for other clients", '<OfferList label="Models for other clients" />'],
    ["models for other clients, other case", "<h3>Models For\n  Other Clients</h3>"],
    ["clients using this key", "<p>Clients using this key will start receiving 401 responses.</p>"],
    ["clients using this key, wrapped", "<p>clients using\n  this key</p>"],
    ["OpenAI-compatible client", "<p>any OpenAI-compatible client</p>"],
    ["OpenAI-compatible client, wrapped", "<p>any OpenAI-compatible\n  client</p>"],
    ["OpenAI-compatible client, other case", 'const s = "an openai-compatible Client";'],
    ["Anthropic-compatible client", "<p>an Anthropic-compatible client</p>"],
    ["use it in a client", "<p>Copy the key and use it in a client.</p>"],
    ["paste this in a client, wrapped", "<p>Paste this in\n  a client.</p>"],
    ["Rotate cache", '{ label: "Rotate cache" }'],
    ["Rotate cache, wrapped", "<button>\n  Rotate\n  cache\n</button>"],
    ["Rotate cache, other case", '{ label: "rotate Cache" }'],
    ["edit this table in Settings", "<p>Operators can edit this table in Settings.</p>"],
    ["edit this table in Settings, wrapped", "<p>Operators can edit this table\n  in Settings.</p>"],
    ["edit this table in Settings, other case", 'const s = "Edit This Table In settings";'],
    ["BURROW_API_KEY", "const env = `export OPENAI_API_KEY=$BURROW_API_KEY`;"],
    ["BURROW_API_KEY in text", "<p>Set <code>BURROW_API_KEY</code> in your shell.</p>"],
    ["BURROW_API_KEY, other case", 'const name = "burrow_api_key";'],
  ];
  it.each(CAUGHT)("catches %s", (_name, src) => {
    expect(retiredIn(src)).toHaveLength(1);
  });

  // What the dashboard may say, and names the user never reads.
  const ALLOWED: [string, string][] = [
    ["Sign in a client", "<h2>Sign in a client</h2>"],
    ["Connect a client", "<h2>Connect a client</h2>"],
    ["a component name", 'import { ClientTokensPanel } from "@/components/ClientTokensPanel";\nconst a = <ClientTokensPanel />;'],
    ["a type name", "const rows: ClientView[] = [];"],
    ["the badge kind status-idle", '<Badge kind="status-idle" nodot>translated</Badge>'],
    ["the connection status closed_idle", 'case "closed_idle": return "status-idle";'],
    ["a client that serves", "<p>A service goes live when a client runs burrow connect for it.</p>"],
    ["clients as machines", "<p>No clients connected</p>"],
    ["clients that use a token", "<p>Clients that use this token will be disconnected.</p>"],
    ["clients using a token", "<p>Clients using this token will be disconnected on the next reconnect.</p>"],
    ["the name a client sends", "<p>The name the client sends when it signs in.</p>"],
    ["what a client asks its local server", "<p>Here the client asks the local server for its model list.</p>"],
    ["other clients that are connected", "<p>No other clients are connected.</p>"],
    ["retired words in a comment", "// an AI service, idle, client offline\nconst a = 1; /* Rotate cache */"],
    ["the new words", "<p>Names apps ask for. Global (cache shared across providers). Online, Offline, Never connected.</p>"],
    ["the gateway key's variable", "const env = `export OPENAI_API_KEY=$BURROW_GATEWAY_KEY`;"],
    ["a neutral key variable for a service", 'const curl = `curl -H "Authorization: Bearer $API_KEY" https://example.com/svc/k7p2qx/`;'],
  ];
  it.each(ALLOWED)("lets %s pass", (_name, src) => {
    expect(retiredIn(src)).toEqual([]);
  });
});

describe("one vocabulary in the dashboard", () => {
  const root = join(__dirname);
  const files = walk(root)
    .filter((p) => /\.tsx?$/.test(p) && !ALLOW.some((re) => re.test(p)))
    .map((p) => relative(root, p));

  it("looks at the pages and components", () => {
    expect(files).toContain(join("pages", "Providers.tsx"));
    expect(files.length).toBeGreaterThan(50);
  });

  it.each(files)("%s", (file) => {
    expect(retiredIn(readFileSync(join(root, file), "utf8")), `${file} uses retired words`).toEqual([]);
  });
});
