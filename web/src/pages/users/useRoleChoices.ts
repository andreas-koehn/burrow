import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "@/lib/api";
import { useCan } from "@/auth/useCan";
import { P } from "@/auth/permissions";
import type { RoleSummary } from "@/lib/contract";
import type { SelectOption } from "@/components/ds";
import { roleOptions } from "./roleOptions";

/**
 * The role options and the description of each role. Without the permission to read roles,
 * while loading, or on an error, the options are User and Admin, plus the user's current role
 * under its plain name (whether it still exists is not known then).
 */
export function useRoleChoices(enabled: boolean, keep?: string): { options: SelectOption[]; describe: (name: string) => string | undefined } {
  const canRead = useCan([P.rolesRead, P.rolesManage]);
  const roles = useQuery({
    queryKey: ["roles"],
    queryFn: () => apiFetch<RoleSummary[]>("/roles"),
    enabled: enabled && canRead,
    retry: false,
  });
  const list = roles.isSuccess ? roles.data : undefined;
  const options = roleOptions(list);
  // The user's current role stays selectable. Only a loaded list can say it was deleted meanwhile.
  if (keep && !options.some((o) => o.value === keep)) {
    options.push({ value: keep, label: list ? `${keep} (no longer exists)` : keep });
  }
  return { options, describe: (name) => list?.find((r) => r.name === name)?.description || undefined };
}
