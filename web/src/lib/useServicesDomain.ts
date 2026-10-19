import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import type { SettingsMap } from "@/lib/contract";

/**
 * The relay's services domain (BURROW_SERVICES_DOMAIN), read from the settings
 * the dashboard already caches. Only a holder of settings:manage can read them;
 * for anyone else, or while the answer is on its way, `domain` is "" and
 * `ready` says whether to trust that.
 */
export function useServicesDomain(): { domain: string; ready: boolean } {
  const allowed = useCan([P.settingsManage]);
  const { data, isPending } = useQuery({
    queryKey: ["settings"],
    queryFn: () => apiFetch<SettingsMap>("/settings"),
    enabled: allowed,
    retry: false,
  });
  return { domain: data?.["services_domain"] ?? "", ready: !allowed || !isPending };
}
