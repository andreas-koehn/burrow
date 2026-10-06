import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { useAuth } from "./useAuth";

export function RequireAuth({ children }: { children: ReactNode }) {
  const { user, loading, error } = useAuth();
  const { pathname, search, hash } = useLocation();
  if (loading) return <div className="p-8 text-sm text-zinc-500">Loading…</div>;
  // The login page returns here afterwards (lib/returnPath.ts), query included:
  // /link?code=… must not lose its code on the way through the login.
  if (error || !user) return <Navigate to="/login" replace state={{ from: pathname + search + hash }} />;
  return <>{children}</>;
}
