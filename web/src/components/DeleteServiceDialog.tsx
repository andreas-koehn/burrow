import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice } from "@/components/ds";
import type { Service } from "@/lib/contract";

export interface DeleteServiceDialogProps {
  service: Pick<Service, "id" | "name">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/** Confirms the deletion of a service. A 409 (a provider still uses it) is shown here, in the dialog. */
export function DeleteServiceDialog({ service, open, onOpenChange }: DeleteServiceDialogProps) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [err, setErr] = useState<string | null>(null);
  const label = service.name || service.id;

  useEffect(() => {
    if (open) setErr(null);
  }, [open]);

  const remove = useMutation({
    mutationFn: () => apiFetch<void>(`/services/${encodeURIComponent(service.id)}`, { method: "DELETE" }),
    onSuccess: () => {
      toast.success(`Service ${label} deleted.`);
      onOpenChange(false);
      // Leave first: the page behind the dialog would ask for the service that is gone.
      navigate("/services");
      void qc.invalidateQueries({ queryKey: ["services"] });
      void qc.invalidateQueries({ queryKey: ["tunnels"] });
      void qc.invalidateQueries({ queryKey: ["ai", "providers"] });
    },
    onError: (e: unknown) => setErr(e instanceof ApiError ? e.message : "Couldn't delete the service."),
  });

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!remove.isPending) onOpenChange(o); }}
      size="sm"
      title={`Delete service · ${label}`}
      description={`Delete ${label} for good?`}
      footer={
        <>
          <Button type="button" variant="secondary" disabled={remove.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button type="button" variant="destructive-solid" disabled={remove.isPending} onClick={() => remove.mutate()}>
            {remove.isPending ? "Deleting…" : "Delete service"}
          </Button>
        </>
      }
    >
      <ErrorNotice variant="warn" role="note">
        The URL stops working. Its API keys, domains and settings are deleted with it, and the
        settings made here do not come back: a client that registers the same tunnel name later
        creates a new service, open to everyone. A connected service cannot be deleted. This
        cannot be undone.
      </ErrorNotice>
      {err && <ErrorNotice role="alert">{err}</ErrorNotice>}
    </Dialog>
  );
}
