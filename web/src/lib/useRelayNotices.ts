import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import { EMAIL_NOT_CONFIGURED } from "@/lib/copy";

export interface RelayNotice {
  id: string;
  message: string;
  action: { label: string; to: string };
}

/**
 * Relay-wide conditions someone with settings:manage should act on. Empty for everybody
 * else, who can neither read the settings nor change them, and empty while the settings are
 * unknown: a notice is never shown on a guess.
 */
export function useRelayNotices(): RelayNotice[] {
  const manages = useCan(P.settingsManage);
  // Same key and fetch as the settings pages, so they share one cache entry.
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: () => apiFetch<Record<string, string>>("/settings"),
    enabled: manages,
    retry: false,
  });
  const emailMissing = manages && settings.data !== undefined && !settings.data["smtp.host"];
  return useMemo(
    () => (emailMissing
      ? [{ id: "email", message: EMAIL_NOT_CONFIGURED, action: { label: "Set up email", to: "/settings/email" } }]
      : []),
    [emailMissing],
  );
}
