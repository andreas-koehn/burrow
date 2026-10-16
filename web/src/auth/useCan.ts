import { useCallback, useMemo } from "react";
import { useAuth } from "./useAuth";
import { holdsAny, permissionsOf } from "./permissions";

/** What the caller may do, while `/me` loads: nothing. */
export function usePermissions(): ReadonlySet<string> {
  const { user } = useAuth();
  return useMemo(() => permissionsOf(user), [user]);
}

/** The same question as a function, for code that asks about many things (navigation, the palette). */
export function useCanFn(): (needs?: readonly string[]) => boolean {
  const held = usePermissions();
  return useCallback((needs?: readonly string[]) => holdsAny(held, needs), [held]);
}

/** Whether the caller holds any of the permissions. */
export function useCan(needs?: string | readonly string[]): boolean {
  const held = usePermissions();
  return holdsAny(held, typeof needs === "string" ? [needs] : needs);
}

/**
 * Whether the caller owns a resource the relay lets only its owner or a caller
 * whose role is admin change (gateway keys: internal/store/ai_gateway_keys.go).
 * This is the relay's own rule, which asks for the role, not a permission.
 */
export function useOwnsOrAdmin(): (ownerId: string) => boolean {
  const { user } = useAuth();
  return useCallback((ownerId: string) => user?.role === "admin" || (user !== undefined && user !== null && user.id === ownerId), [user]);
}
