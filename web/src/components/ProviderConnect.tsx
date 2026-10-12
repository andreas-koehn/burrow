import { useId } from "react";
import { CopyButton, CopyableCode } from "@/components/CopyableCode";
import { claudeCodeSettings, pickTier } from "@/lib/connectSnippets";
import type { AiProvider } from "@/lib/contract";

export interface ProviderConnectProps {
  /** The provider's address, ending in /v1. */
  baseUrl: string;
  /** A model this provider serves, used in the example. */
  exampleModel?: string;
  /** The format the provider speaks; OpenAI when left out. */
  apiFormat?: AiProvider["api_format"];
}

/**
 * What an app needs to use a provider: base URL plus ready examples in the
 * format the provider speaks. Where the key goes there is a placeholder or the
 * $BURROW_API_KEY variable; a real key is never shown.
 */
export function ProviderConnect({ baseUrl, exampleModel, apiFormat = "openai" }: ProviderConnectProps) {
  const titleId = useId();
  const anthropic = apiFormat === "anthropic";
  // An Anthropic app adds /v1/messages to its base URL itself.
  const shown = anthropic ? baseUrl.replace(/\/v1\/?$/, "") : baseUrl;
  const model = exampleModel || "<model>";
  const curl = [
    `curl ${baseUrl}/chat/completions \\`,
    `  -H "Authorization: Bearer $BURROW_API_KEY" \\`,
    `  -H "Content-Type: application/json" \\`,
    `  -d '{"model": "${model}", "messages": [{"role": "user", "content": "Hello"}]}'`,
  ].join("\n");
  const env = [
    `export OPENAI_BASE_URL=${baseUrl}`,
    `export OPENAI_API_KEY=$BURROW_API_KEY`,
  ].join("\n");

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
          <CopyableCode text={curl} label="Copy curl example" wrap />
          <CopyableCode text={env} label="Copy environment variables" wrap />
        </>
      )}
    </section>
  );
}
