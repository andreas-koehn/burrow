import type { SelectOption } from "@/components/ds";
import type { RoleSummary } from "@/lib/contract";

const BUILTIN: SelectOption[] = [
  { value: "admin", label: "Admin" },
  { value: "user", label: "User" },
];

/** The role choices of a user dialog: the built-in roles first, then the custom ones by name. */
export function roleOptions(roles: RoleSummary[] | undefined): SelectOption[] {
  if (!roles) return [{ value: "user", label: "User" }, { value: "admin", label: "Admin" }];
  const custom = roles
    .filter((r) => r.name !== "admin" && r.name !== "user")
    .sort((a, b) => a.name.localeCompare(b.name))
    .map((r) => ({ value: r.name, label: r.name }));
  return [...BUILTIN, ...custom];
}
