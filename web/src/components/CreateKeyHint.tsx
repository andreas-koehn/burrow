import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import type { AiGatewayKey } from "@/lib/contract";

export interface CreateKeyHintProps {
  /** Slug of the provider the snippet is for: the new key starts limited to it. */
  provider?: string;
}

/**
 * Beside a snippet: nothing while the reader has a gateway key that works,
 * otherwise one line that leads to "New gateway key". Nothing while the list
 * loads and nothing when it cannot be read, so the line neither flashes nor
 * turns into an error for someone who may not list keys.
 */
export function CreateKeyHint({ provider }: CreateKeyHintProps) {
  // Same key and fetch as the Gateway keys page.
  const keys = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
  });
  if (!Array.isArray(keys.data) || keys.data.some((k) => !k.revoked_at)) return null;

  const query = new URLSearchParams({ new: "1", ...(provider ? { provider } : {}) });
  return (
    <p className="row row-center gap-2 muted small">
      <span>You have no gateway key yet.</span>
      <Link className="btn btn-secondary btn-sm" to={`/gateway/keys?${query}`}>Create a gateway key</Link>
    </p>
  );
}
