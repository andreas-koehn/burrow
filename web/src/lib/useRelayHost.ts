import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";

interface ConnectInfo { server: string }

/**
 * The relay's host, for an address a visitor can use. A 404 is tolerated: the
 * dashboard's own hostname stands in. `enabled` keeps a page that has no use
 * for it from asking.
 */
export function useRelayHost(enabled = true): string {
  const info = useQuery({
    queryKey: ["connect-info"],
    queryFn: () => apiFetch<ConnectInfo>("/clients/connect-info"),
    retry: false,
    staleTime: 5 * 60_000,
    enabled,
  });
  const winHost = typeof window !== "undefined" ? window.location.hostname : "";
  const s = info.data?.server ?? "";
  // ":7000" (port only, relay bound to all interfaces) carries no host.
  const i = s.lastIndexOf(":");
  return i <= 0 ? winHost : s.slice(0, i);
}
