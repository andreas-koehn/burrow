import { useId } from "react";
import { Copy } from "lucide-react";
import { toast } from "sonner";

export interface ProviderConnectProps {
  baseUrl: string;
  /** A model this provider serves, used in the example. */
  exampleModel?: string;
}

function copy(text: string) {
  void navigator.clipboard?.writeText(text);
  // Shown by the toaster of the page this is used on.
  toast.success("Copied.");
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
        <button
          type="button"
          className="icon-btn"
          aria-label={`Copy base URL ${baseUrl}`}
          onClick={() => copy(baseUrl)}
        >
          <Copy size={13} aria-hidden="true" />
        </button>
      </div>
      <div className="row gap-2">
        <pre className="cmd-block wrap flex-1"><code>{curl}</code></pre>
        <button type="button" className="icon-btn" aria-label="Copy curl example" onClick={() => copy(curl)}>
          <Copy size={13} aria-hidden="true" />
        </button>
      </div>
      <div className="row gap-2">
        <pre className="cmd-block wrap flex-1"><code>{env}</code></pre>
        <button type="button" className="icon-btn" aria-label="Copy environment variables" onClick={() => copy(env)}>
          <Copy size={13} aria-hidden="true" />
        </button>
      </div>
    </section>
  );
}
