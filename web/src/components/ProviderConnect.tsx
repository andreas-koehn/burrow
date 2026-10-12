import { useId } from "react";
import { CopyButton, CopyableCode } from "@/components/CopyableCode";
import { CreateKeyHint } from "@/components/CreateKeyHint";
import { claudeCodeSettings, curlChat, openAiEnv, pickTier } from "@/lib/connectSnippets";
import type { AiProvider } from "@/lib/contract";

export interface ProviderConnectProps {
  /** The provider's address, ending in /v1. */
  baseUrl: string;
  /** A model this provider serves, used in the example. */
  exampleModel?: string;
  /** The format the provider speaks; OpenAI when left out. */
  apiFormat?: AiProvider["api_format"];
  /** The provider's slug: a key created from here starts limited to it. */
  provider?: string;
}

/**
 * What an app needs to use a provider: base URL plus ready examples in the
 * format the provider speaks. Where the key goes there is a placeholder or the
 * $BURROW_GATEWAY_KEY variable; a real key is never shown.
 */
export function ProviderConnect({ baseUrl, exampleModel, apiFormat = "openai", provider }: ProviderConnectProps) {
  const titleId = useId();
  const anthropic = apiFormat === "anthropic";
  // An Anthropic app adds /v1/messages to its base URL itself.
  const shown = anthropic ? baseUrl.replace(/\/v1\/?$/, "") : baseUrl;
  const model = exampleModel || "<model>";

  return (
    <section className="card provider-connect" aria-labelledby={titleId}>
      <h2 id={titleId}>Use it in an app</h2>
      <p className="muted small">
        Use this as the base URL of any {anthropic ? "Anthropic-compatible" : "OpenAI-compatible"} app, with a gateway key.
      </p>
      <div className="row row-center gap-2 service-url">
        <code className="mono service-url-path">{shown}</code>
        <CopyButton text={shown} label={`Copy base URL ${shown}`} />
      </div>
      {anthropic ? (
        <>
          <CopyableCode
            text={claudeCodeSettings(shown, pickTier(exampleModel ? [exampleModel] : []))}
            label="Copy settings for Claude Code"
            wrap
          />
          <p className="muted small">
            For Claude Code: put this into <code>~/.claude/settings.json</code>, then replace the key placeholder.
          </p>
        </>
      ) : (
        <>
          <CopyableCode text={curlChat(baseUrl, model)} label="Copy curl example" wrap />
          <CopyableCode text={openAiEnv(baseUrl)} label="Copy environment variables" wrap />
        </>
      )}
      <CreateKeyHint provider={provider} />
    </section>
  );
}
