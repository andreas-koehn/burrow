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
