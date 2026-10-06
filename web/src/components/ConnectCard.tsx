import { useId, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { Tabs } from "@/components/ds";
import { CopyButton, CopyableCode } from "@/components/CopyableCode";
import {
  claudeCodeSettings, codexConfig, genericProvider, openAiSdkPython, openAiSdkTypeScript, pickTier, KEY_ENV,
} from "@/lib/connectSnippets";
import { dialectBaseUrl } from "@/lib/serviceUrl";
import type { Dialect, GatewayInfo } from "@/lib/contract";

export interface ConnectCardProps {
  endpoints: GatewayInfo["endpoints"];
  /** Enabled models only; the caller filters. */
  models: { name: string; dialects: Dialect[] }[];
}

const FORMAT: Record<Dialect, string> = { openai: "OpenAI", anthropic: "Anthropic" };

function BaseUrl({ url }: { url: string }) {
  return (
    <div className="row row-center gap-2 service-url">
      <code className="mono service-url-path">{url}</code>
      <CopyButton text={url} label={`Copy base URL ${url}`} />
    </div>
  );
}

/**
 * Ready configurations for the clients people point at the gateway. Every one
 * carries a placeholder or an environment variable where the key goes: the
 * card never receives a key, so it cannot show one.
 */
export function ConnectCard({ endpoints, models }: ConnectCardProps) {
  const titleId = useId();
  const [tab, setTab] = useState("claude-code");
  const base = (d: Dialect) => dialectBaseUrl(d, endpoints.find((e) => e.dialect === d)?.base_url);
  const served = (d: Dialect) => models.filter((m) => m.dialects.includes(d)).map((m) => m.name);
  const openai = served("openai");
  const anthropic = served("anthropic");
  const leftOut = models.filter((m) => !m.dialects.includes("anthropic")).map((m) => m.name);

  // A client speaks one format: without a model in it there is nothing to configure.
  const forFormat = (d: Dialect, names: string[], content: () => ReactNode) =>
    names.length > 0 ? content() : (
      <p className="muted">
        No model is served in the {FORMAT[d]} format yet. Give a model a target in this format on the{" "}
        <Link className="link-inline" to="/gateway/models">Models</Link> page.
      </p>
    );

  return (
    <section className="card provider-connect connect-card" aria-labelledby={titleId}>
      <h2 id={titleId}>Connect a client</h2>
      {models.length === 0 ? (
        <p className="muted">Create a model first, then come back for a ready configuration.</p>
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
                    text={claudeCodeSettings(base("anthropic"), pickTier(anthropic))}
                    label="Copy settings for Claude Code"
                  />
                  <p className="muted small">
                    Put this into <code>~/.claude/settings.json</code>, then replace the key placeholder.
                  </p>
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
              content: forFormat("openai", openai, () => (
                <>
                  <BaseUrl url={base("openai")} />
                  <CopyableCode text={codexConfig(base("openai"), openai[0]!)} label="Copy config for Codex" />
                  <p className="muted small">
                    Put this into <code>~/.codex/config.toml</code>. Set <code>{KEY_ENV}</code> in your shell.
                  </p>
                  <p className="muted small">
                    Codex needs a provider that offers the Responses API. Tick that option on the provider behind the model.
                  </p>
                </>
              )),
            },
            {
              value: "openai-sdk",
              label: "OpenAI SDK",
              content: forFormat("openai", openai, () => (
                <>
                  <BaseUrl url={base("openai")} />
                  <CopyableCode text={openAiSdkPython(base("openai"), openai[0]!)} label="Copy Python example" />
                  <CopyableCode text={openAiSdkTypeScript(base("openai"), openai[0]!)} label="Copy TypeScript example" />
                  <p className="muted small">Set <code>{KEY_ENV}</code> in your shell.</p>
                </>
              )),
            },
            {
              value: "other",
              label: "Other",
              content: forFormat("openai", openai, () => (
                <>
                  <p className="muted small">
                    What a client asks for when you add an OpenAI-compatible provider by hand.
                  </p>
                  <dl className="def-list">
                    {genericProvider(base("openai"), openai).map((row) => (
                      <div key={row.label} className="def-row">
                        <dt className="def-key">{row.label}</dt>
                        <dd className="def-val">{row.value}</dd>
                        <CopyButton text={row.value} label={`Copy ${row.label}`} />
                      </div>
                    ))}
                  </dl>
                </>
              )),
            },
          ]}
        />
      )}
    </section>
  );
}
