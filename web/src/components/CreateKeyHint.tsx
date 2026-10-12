import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { useAuth } from "@/auth/useAuth";
import type { AiGatewayKey } from "@/lib/contract";

export interface CreateKeyHintProps {
  /** Slug of the provider the snippet is for: the new key starts limited to it. */
  provider?: string;
}

/**
 * Beside a snippet: nothing while the reader has a gateway key that works
 * there, otherwise one line that leads to "New gateway key". Nothing while the
 * answers load and nothing when they cannot be read, so the line neither
 * flashes nor turns into an error for someone who may not list keys.
 */
export function CreateKeyHint({ provider }: CreateKeyHintProps) {
  const { user } = useAuth();
  // Same key and fetch as the Gateway keys page.
  const keys = useQuery({
    queryKey: ["ai", "keys"],
    queryFn: () => apiFetch<AiGatewayKey[]>("/ai/keys"),
    retry: false,
  });
  if (!user || !Array.isArray(keys.data)) return null;

  // An admin is sent every user's keys; only the reader's own can go into their app.
  const mine = keys.data.filter((k) => k.user_id === user.id && !k.revoked_at);
  // A provider's own address /ai/<provider>/… takes a key for every model or
  // for everything from that provider, not one limited to model names.
  const opens = (k: AiGatewayKey) =>
    provider === undefined || k.allowed_models.length === 0 || k.allowed_models.includes(`${provider}/*`);
  if (mine.some(opens)) return null;

  const query = new URLSearchParams({ new: "1", ...(provider ? { provider } : {}) });
  return (
    <p className="row row-center gap-2 muted small">
      <span>{mine.length === 0 ? "You have no gateway key yet." : "None of your gateway keys opens this provider."}</span>
      <Link className="btn btn-secondary btn-sm" to={`/gateway/keys?${query}`}>Create a gateway key</Link>
    </p>
  );
}
