import type { ReactNode } from "react";
import { Navigate } from "react-router-dom";
import { useAuth } from "@/auth/useAuth";

/** Admin-only settings pages send everyone else to their own profile. */
export function RequireAdmin({ children }: { children: ReactNode }) {
  const { user, loading } = useAuth();
  if (loading || !user) return null; // RequireAuth above shows the loading state and handles a missing user
  return user.role === "admin" ? <>{children}</> : <Navigate to="/settings/profile" replace />;
}
