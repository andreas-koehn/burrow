import { useId } from "react";
import { CopyButton, CopyableCode } from "@/components/CopyableCode";

export interface ProviderConnectProps {
  baseUrl: string;
  /** A model this provider serves, used in the example. */
  exampleModel?: string;
}

/**
 * What a client needs to use a provider: base URL plus ready examples. The
 * key is always the $BURROW_API_KEY placeholder; a real key is never shown.
 */
export function ProviderConnect({ baseUrl, exampleModel }: ProviderConnectProps) {
  const titleId = useId();
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
      <h2 id={titleId}>Connect a client</h2>
      <p className="muted small">
        Use this as the base URL of any OpenAI-compatible client, with one of this provider's API keys.
      </p>
      <div className="row row-center gap-2 service-url">
        <code className="mono service-url-path">{baseUrl}</code>
        <CopyButton text={baseUrl} label={`Copy base URL ${baseUrl}`} />
      </div>
      <CopyableCode text={curl} label="Copy curl example" wrap />
      <CopyableCode text={env} label="Copy environment variables" wrap />
    </section>
  );
}
