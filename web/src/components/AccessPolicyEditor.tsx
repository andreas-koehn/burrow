import { useEffect, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button } from "@/components/ds";
import { Toaster } from "@/components/ui/sonner";
import { toast } from "sonner";
import type { RoleSummary, AccessPolicy } from "@/lib/contract";

/**
 * `forward` is the policy of a tcp service: the roles besides the owner that may
 * forward to it with `burrow forward`. The owner always may, so an empty policy is
 * not a locked door.
 */
export function AccessPolicyEditor({ serviceId, forward = false }: { serviceId: string; forward?: boolean }) {
  const qc = useQueryClient();

  const { data: roles } = useQuery({
    queryKey: ["roles"],
    queryFn: () => apiFetch<RoleSummary[]>("/roles"),
    staleTime: 30_000,
  });
  const { data: policy } = useQuery({
    queryKey: ["service-access-policy", serviceId],
    queryFn: () => apiFetch<AccessPolicy>(`/services/${serviceId}/access-policy`),
  });

  const [selected, setSelected] = useState<string[] | null>(null);
  // Seed local selection once the server policy arrives.
  useEffect(() => {
    if (policy && selected === null) setSelected(policy.roles);
  }, [policy, selected]);

  const current = selected ?? policy?.roles ?? [];
  const [err, setErr] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: () =>
      apiFetch(`/services/${serviceId}/access-policy`, {
        method: "PUT",
        body: JSON.stringify({ roles: current }),
      }),
    onSuccess: () => {
      setErr(null);
      qc.invalidateQueries({ queryKey: ["service-access-policy", serviceId] });
      qc.invalidateQueries({ queryKey: ["service", serviceId] });
      qc.invalidateQueries({ queryKey: ["services"] });
      toast.success("Access policy saved.");
    },
    onError: (e: unknown) =>
      setErr(e instanceof ApiError ? e.message : "Couldn't save the access policy."),
  });

  function toggle(name: string) {
    setSelected((prev) => {
      const base = prev ?? policy?.roles ?? [];
      return base.includes(name) ? base.filter((r) => r !== name) : [...base, name];
    });
  }

  return (
    <div className="access-policy-editor">
      <h3>{forward ? "Who may forward" : "Access policy"}</h3>
      <div className="chip-row" role="group" aria-label="Allowed roles">
        {(roles ?? []).map((r) => {
          const on = current.includes(r.name);
          return (
            <button
              key={r.name}
              type="button"
              className={on ? "chip is-on" : "chip"}
              aria-pressed={on}
              onClick={() => toggle(r.name)}
            >
              {r.name}
            </button>
          );
        })}
      </div>

      {current.length === 0 && (
        forward ? (
          <p className="muted" role="status">Empty policy: only the owner can forward to this service.</p>
        ) : (
          <p className="notice-inline warn" role="status">
            Empty policy — nobody can reach this service until a role is added.
          </p>
        )
      )}

      <p className="muted">
        {forward
          ? "Signed-in users whose role is listed may forward to this service with burrow forward. The owner always may."
          : "Signed-in users whose role is listed may reach this service — SSO: one Burrow login covers every protected service their role allows."}
      </p>

      {err && <p role="alert" className="notice-inline">{err}</p>}

      <div className="actions">
        <Button variant="primary" size="sm" disabled={save.isPending} onClick={() => save.mutate()}>
          {save.isPending ? "Saving…" : "Save"}
        </Button>
      </div>
      <Toaster />
    </div>
  );
}
