import { useAuth } from "@/auth/useAuth";

/**
 * The relay's services domain (BURROW_SERVICES_DOMAIN), read from `/me`, which
 * every signed-in user gets. `domain` is "" when the relay has none, or while
 * the answer is on its way; `ready` says whether to trust that.
 */
export function useServicesDomain(): { domain: string; ready: boolean } {
  const { user, loading } = useAuth();
  return { domain: user?.services_domain ?? "", ready: !loading };
}
