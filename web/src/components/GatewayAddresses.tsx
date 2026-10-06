import { Copy } from "lucide-react";
import { dialectBaseUrl, urlPath } from "@/lib/serviceUrl";
import type { Dialect, GatewayInfo } from "@/lib/contract";

const DIALECTS: { dialect: Dialect; label: string }[] = [
  { dialect: "openai", label: "OpenAI" },
  { dialect: "anthropic", label: "Anthropic" },
];

export interface GatewayAddressesProps {
  /** As GET /ai/gateway reports them; a missing or empty one falls back to this origin. */
  endpoints?: GatewayInfo["endpoints"];
}

/** The gateway's base URL per API format: shows the path, copies the full URL. */
export function GatewayAddresses({ endpoints = [] }: GatewayAddressesProps) {
  return (
    <ul className="gateway-addresses" aria-label="Gateway base URLs">
      {DIALECTS.map(({ dialect, label }) => {
        const full = dialectBaseUrl(dialect, endpoints.find((e) => e.dialect === dialect)?.base_url);
        return (
          <li key={dialect} className="row row-center gap-2 service-url">
            <span className="muted small">{label}</span>
            <span className="mono service-url-path" title={full}>{urlPath(full)}</span>
            <button
              type="button"
              className="icon-btn"
              aria-label={`Copy URL ${full}`}
              onClick={() => void navigator.clipboard?.writeText(full)}
            >
              <Copy size={13} aria-hidden="true" />
            </button>
          </li>
        );
      })}
    </ul>
  );
}
