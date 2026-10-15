import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";

export interface Me { id: string; email: string; role: string; permissions?: string[] }

export function useAuth() {
  const q = useQuery({ queryKey: ["me"], queryFn: () => apiFetch<Me>("/me"), staleTime: 30_000 });
  return { user: q.data, loading: q.isPending, error: q.error as Error | null };
}
