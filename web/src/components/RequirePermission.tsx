import type { ReactNode } from "react";
import { useAuth } from "@/auth/useAuth";
import { useCan } from "@/auth/useCan";
import { PageHeader } from "@/components/ds";

/** "webhooks:manage" / "users:read or users:manage" — what a page says it needs. */
function missing(needs: readonly string[]): string {
  if (needs.length === 1) return `It needs the permission ${needs[0]}. Ask an admin to add it to your role.`;
  return `It needs one of the permissions ${needs.slice(0, -1).join(", ")} or ${needs[needs.length - 1]}.`;
}

/**
 * A page for people who hold any of `needs`. Opened by link without one, it says which
 * permission is missing rather than showing an empty page.
 */
export function RequirePermission({ needs, children }: { needs: readonly string[]; children: ReactNode }) {
  const { user, loading } = useAuth();
  const allowed = useCan(needs);
  if (loading || !user) return null; // RequireAuth above shows the loading state and handles a missing user
  if (allowed) return <>{children}</>;
  return (
    <PageHeader
      title="You cannot open this page"
      subtitle={missing(needs)}
      back={{ to: "/settings/profile", label: "your profile" }}
    />
  );
}
