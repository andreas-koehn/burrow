import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";

export interface Me {
  id: string;
  email: string;
  role: string;
  permissions?: string[];
  /** BURROW_SERVICES_DOMAIN, "" when unset (absent from an older relay). */
  services_domain?: string;
}

export function useAuth() {
  const q = useQuery({ queryKey: ["me"], queryFn: () => apiFetch<Me>("/me"), staleTime: 30_000 });
  return { user: q.data, loading: q.isPending, error: q.error as Error | null };
}
