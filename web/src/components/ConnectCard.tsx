import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Tabs } from "@/components/ds";
import { CopyButton, CopyableCode } from "@/components/CopyableCode";
import { CreateKeyHint } from "@/components/CreateKeyHint";
import {
  claudeCodeSettings, codexConfig, genericProvider, openAiSdkPython, openAiSdkTypeScript, pickTier, KEY_ENV,
} from "@/lib/connectSnippets";
import { dialectBaseUrl } from "@/lib/serviceUrl";
import { GATEWAY_KEY_CLIENT, mcpOAuthSnippets, mcpSnippets } from "@/lib/mcpSnippets";
import { Badge } from "@/components/ds";
import { towardMessages } from "@/lib/translation";
import type { AiModel, Dialect, DialectMode, GatewayInfo } from "@/lib/contract";

/** What the card needs of a model: its name and how each format is served. */
export type ConnectModel = Pick<AiModel, "name" | "dialect_modes" | "responses_mode" | "translation_pairs">;

export interface ConnectCardProps {
  endpoints: GatewayInfo["endpoints"];
  /** Enabled models only; the caller filters. */
  models: ConnectModel[];
  /** Whether the reader may create a model; decides what the card says when there is none. Default true. */
  canCreate?: boolean;
}

const FORMAT: Record<Dialect, string> = { openai: "OpenAI", anthropic: "Anthropic" };

/** What an app asks for: a format's main endpoint, or the Responses API on the OpenAI endpoint. */
type Endpoint = Dialect | "responses";

/** A model a tab offers. */
interface Offer {
  name: string;
  translated: boolean;
  /** The pairs a translated request can go through, in the order they are tried. */
  pairs?: string[];
}

const modeOf = (m: ConnectModel, e: Endpoint): DialectMode | undefined =>
  (e === "responses" ? m.responses_mode : m.dialect_modes?.[e]);

/** The models served on an endpoint: natively served ones first, then translated ones, each in list order. */
function offers(models: ConnectModel[], e: Endpoint): Offer[] {
  const of = (mode: DialectMode) => models.filter((m) => modeOf(m, e) === mode)
    .map((m) => ({ name: m.name, translated: mode === "translated", pairs: m.translation_pairs?.[e] }));
  return [...of("native"), ...of("translated")];
}

/** The models a tab's configuration can name; a translated one says so in words. */
function OfferList({ label, list }: { label: string; list: Offer[] }) {
  return (
    <ul className="connect-models" aria-label={label}>
      {list.map((m) => (
        <li key={m.name}>
          <span className="mono">{m.name}</span>
          {m.translated && <Badge nodot kind="status-idle">translated</Badge>}
        </li>
      ))}
    </ul>
  );
}

/** The media and rerank paths, served on the OpenAI base URL besides chat, embeddings and responses. */
const MEDIA_LINE = "Also: /v1/images/generations, /v1/audio/speech, /v1/audio/transcriptions, /v1/audio/translations, /v1/rerank";

const DROPPED_HEADER = <>Every translated response names what was left out in its <code>Burrow-Dropped</code> header.</>;

/** What a translated model loses behind an Anthropic-format provider, for apps that speak an OpenAI format. */
function TowardMessagesNote({ list }: { list: Offer[] }) {
  // Any candidate counts, not only the first: a later one can be the one that answers.
  if (!list.some((m) => m.translated && towardMessages(m.pairs))) return null;
  return (
    <p className="muted small">
      A translated model may be answered by an Anthropic-format provider: there the output is capped at 32,000 tokens
      when the app sets no limit, and temperature and top_p are not sent.
    </p>
  );
}

function BaseUrl({ url }: { url: string }) {
  return (
    <div className="row row-center gap-2 service-url">
      <code className="mono service-url-path">{url}</code>
      <CopyButton text={url} label={`Copy base URL ${url}`} />
    </div>
  );
}

/** The relay's origin as the endpoints show it; the page's own when the relay has no auth domain. */
function relayOrigin(endpoints: GatewayInfo["endpoints"]): string {
  for (const e of endpoints) {
    try {
      if (e.base_url) return new URL(e.base_url).origin;
    } catch { /* not a URL: try the next */ }
  }
  return window.location.origin;
}

/** The two ways to connect an MCP client to /mcp: by URL (OAuth) or with a gateway key as a header. */
function McpTab({ url }: { url: string }) {
  const [client, setClient] = useState("claude-code");
  const oauth = mcpOAuthSnippets(url);
  const key = mcpSnippets(url, GATEWAY_KEY_CLIENT);
  const both = (o: string, k: string, label: string) => (
    <>
      <p className="muted small">If the client can sign in (OAuth), give it the address alone. It opens a browser and asks you to approve the tools.</p>
      <CopyableCode text={o} label={`Copy ${label} for sign-in`} wrap />
      <p className="muted small">Otherwise send a gateway key as a header. Put the key into <code>{GATEWAY_KEY_CLIENT.env}</code> first.</p>
      <CopyableCode text={k} label={`Copy ${label} with a gateway key`} wrap />
    </>
  );
  return (
    <>
      <BaseUrl url={url} />
      <Tabs
        label="MCP clients"
        value={client}
        onChange={setClient}
        tabs={[
          { value: "claude-code", label: "Claude Code", content: both(oauth.claudeCode, key.claudeCode, "Claude Code command") },
          {
            value: "cursor",
            label: "Cursor",
            content: (<><p className="muted small mono">~/.cursor/mcp.json</p>{both(oauth.cursor, key.cursor, "Cursor configuration")}</>),
          },
          {
            value: "vscode",
            label: "VS Code",
            content: (<><p className="muted small mono">.vscode/mcp.json</p>{both(oauth.vscode, key.vscode, "VS Code configuration")}</>),
          },
          {
            value: "other",
            label: "Other",
            content: (
              <>
                <p className="muted small">A client that speaks MCP over Streamable HTTP takes the address, and either signs in or sends this header.</p>
                <CopyableCode text={"Authorization: Bearer <your gateway key>"} label="Copy the header" />
              </>
            ),
          },
        ]}
      />
      <p className="muted small">
        A key reaches only the tools it lists; a key with no tools reaches none. Set the tools when you create the key, or edit them on the{" "}
        <Link className="link-inline" to="/gateway/keys">Gateway keys</Link> page. Tool sources are on the{" "}
        <Link className="link-inline" to="/gateway/tools">Tools</Link> page.
      </p>
    </>
  );
}

/**
 * Ready configurations for the apps people point at the gateway. Every one
 * carries a placeholder or an environment variable where the key goes: the
 * card never receives a key, so it cannot show one.
 */
export function ConnectCard({ endpoints, models, canCreate = true }: ConnectCardProps) {
  const titleId = useId();
  const [tab, setTab] = useState("claude-code");
  const mcpUrl = `${relayOrigin(endpoints)}/mcp`;
  const base = (d: Dialect) => dialectBaseUrl(d, endpoints.find((e) => e.dialect === d)?.base_url);
  // A model is offered where it is served: by a target of that format, or translated into it.
  const anthropic = offers(models, "anthropic");
  const chat = offers(models, "openai");
  const responses = offers(models, "responses");
  const names = (list: Offer[]) => list.map((m) => m.name);
  const notServed = (e: Endpoint) => models.filter((m) => !offers([m], e).length).map((m) => m.name);
  const leftOut = notServed("anthropic");
  const noResponses = notServed("responses");
  // Claude Code pins a model per tier; one of them translated is enough.
  const tiers = pickTier(names(anthropic));
  const pinnedTranslated = anthropic.some((m) => m.translated && Object.values(tiers).includes(m.name));

  // An app speaks one format: without a model in it there is nothing to configure.
  const forFormat = (d: Dialect, list: Offer[], content: () => ReactNode) =>
    list.length > 0 ? content() : (
      <p className="muted">
        No model is served in the {FORMAT[d]} format yet.{" "}
        {canCreate ? "Give a model a target in this format, or turn on translation for it," : "An administrator can give a model a target in this format, or turn on translation for it,"} on the{" "}
        <Link className="link-inline" to="/gateway/models">Models</Link> page.
      </p>
    );

  return (
    <section className="card provider-connect connect-card" aria-labelledby={titleId}>
      <h2 id={titleId}>Connect a client</h2>
      {models.length === 0 ? (
        <>
          <p className="muted">
            {canCreate
              ? "Create a model first, then come back for a ready configuration."
              : "There is no model to ask for yet. Ask an administrator to create a model, then come back for a ready configuration."}
          </p>
          {/* Tools need no model. */}
          <h3 className="small">MCP</h3>
          <McpTab url={mcpUrl} />
        </>
      ) : (
        <Tabs
          value={tab}
          onChange={setTab}
          tabs={[
            {
              value: "claude-code",
              label: "Claude Code",
              content: forFormat("anthropic", anthropic, () => (
                <>
                  <BaseUrl url={base("anthropic")} />
                  <CopyableCode
                    text={claudeCodeSettings(base("anthropic"), tiers, { thinkingOff: pinnedTranslated })}
                    label="Copy settings for Claude Code"
                  />
                  <p className="muted small">
                    Put this into <code>~/.claude/settings.json</code>, then replace the key placeholder.
                  </p>
                  <OfferList label="Models for Claude Code" list={anthropic} />
                  {anthropic.some((m) => m.translated) && (
                    <p className="muted small">
                      A translated model is answered by a provider that speaks the OpenAI format: prompt caching hints,
                      thinking signatures and beta features are not carried over. {DROPPED_HEADER}
                    </p>
                  )}
                  {pinnedTranslated && (
                    <p className="muted small">
                      Translated models run without extended thinking; these settings turn it off, for every model
                      in them. Remove <code>MAX_THINKING_TOKENS</code> to keep it for the models that are served natively.
                    </p>
                  )}
                  {leftOut.length > 0 && (
                    <p className="muted small">
                      {leftOut.length === 1
                        ? `1 model is not served in the Anthropic format: ${leftOut[0]}`
                        : `${leftOut.length} models are not served in the Anthropic format: ${leftOut.join(", ")}`}
                    </p>
                  )}
                </>
              )),
            },
            {
              value: "codex",
              label: "Codex",
              content: responses.length === 0 ? (
                <p className="muted">
                  No model is served on the Responses API yet, and Codex speaks nothing else.{" "}
                  {canCreate
                    ? "Tick that option on a provider behind a model, or turn on translation for the model,"
                    : "An administrator can tick that option on a provider behind a model, or turn on translation for the model,"} on the{" "}
                  <Link className="link-inline" to="/gateway/models">Models</Link> page.
                </p>
              ) : (
                <>
                  <BaseUrl url={base("openai")} />
                  <CopyableCode text={codexConfig(base("openai"), responses[0]!.name)} label="Copy config for Codex" />
                  <p className="muted small">
                    Put this into <code>~/.codex/config.toml</code>. Set <code>{KEY_ENV}</code> in your shell.
                  </p>
                  <OfferList label="Models for Codex" list={responses} />
                  <p className="muted small">
                    Codex speaks the Responses API. A model is offered here when a provider behind it offers that API, or
                    when translation is on for the model.
                  </p>
                  {responses.some((m) => m.translated) && (
                    <p className="muted small">
                      A translated model is answered by a provider that does not offer the Responses API. Burrow keeps
                      no response state (<code>previous_response_id</code> and <code>store</code> are left out), and
                      tools that run at the provider, such as web search, are not offered to the model. {DROPPED_HEADER}
                    </p>
                  )}
                  <TowardMessagesNote list={responses} />
                  {noResponses.length > 0 && (
                    <p className="muted small">
                      {noResponses.length === 1
                        ? `1 model is not served on the Responses API: ${noResponses[0]}`
                        : `${noResponses.length} models are not served on the Responses API: ${noResponses.join(", ")}`}
                    </p>
                  )}
                </>
              ),
            },
            {
              value: "openai-sdk",
              label: "OpenAI SDK",
              content: forFormat("openai", chat, () => (
                <>
                  <BaseUrl url={base("openai")} />
                  <CopyableCode text={openAiSdkPython(base("openai"), chat[0]!.name)} label="Copy Python example" />
                  <CopyableCode text={openAiSdkTypeScript(base("openai"), chat[0]!.name)} label="Copy TypeScript example" />
                  <p className="muted small">Set <code>{KEY_ENV}</code> in your shell.</p>
                  <p className="muted small">{MEDIA_LINE}</p>
                  <OfferList label="Models for the OpenAI SDK" list={chat} />
                  {chat.some((m) => m.translated) && <p className="muted small">{DROPPED_HEADER}</p>}
                  <TowardMessagesNote list={chat} />
                </>
              )),
            },
            {
              value: "other",
              label: "Other",
              content: forFormat("openai", chat, () => (
                <>
                  <p className="muted small">
                    What an app asks for when you add an OpenAI-compatible provider by hand.
                  </p>
                  <dl className="def-list">
                    {genericProvider(base("openai"), names(chat)).map((row) => (
                      <div key={row.label} className="def-row">
                        <dt className="def-key">{row.label}</dt>
                        <dd className="def-val">{row.value}</dd>
                        <CopyButton text={row.value} label={`Copy ${row.label}`} />
                      </div>
                    ))}
                  </dl>
                  <p className="muted small">{MEDIA_LINE}</p>
                  <OfferList label="Models for other apps" list={chat} />
                  {chat.some((m) => m.translated) && <p className="muted small">{DROPPED_HEADER}</p>}
                  <TowardMessagesNote list={chat} />
                </>
              )),
            },
            { value: "mcp", label: "MCP", content: <McpTab url={mcpUrl} /> },
          ]}
        />
      )}
      {/* Under the tabs, so it stands below whichever snippet is open; without a model there is none. */}
      {models.length > 0 && <CreateKeyHint />}
    </section>
  );
}
