import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice } from "@/components/ds";
import { SlugField, slugError } from "@/components/SlugField";
import type { Service } from "@/lib/contract";

export interface EditSlugDialogProps {
  service: Pick<Service, "id" | "name" | "slug">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function EditSlugDialog({ service, open, onOpenChange }: EditSlugDialogProps) {
  const qc = useQueryClient();
  const [slug, setSlug] = useState(service.slug);
  const [serverErr, setServerErr] = useState<string | null>(null);

  useEffect(() => {
    if (open) { setSlug(service.slug); setServerErr(null); }
  }, [open, service.slug]);

  const save = useMutation({
    mutationFn: () =>
      apiFetch<{ slug: string; url: string }>(`/services/${service.id}/slug`, {
        method: "PUT",
        body: JSON.stringify({ slug }),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["services"] });
      qc.invalidateQueries({ queryKey: ["service", service.id] });
      qc.invalidateQueries({ queryKey: ["tunnels"] });
      onOpenChange(false);
    },
    onError: (e: unknown) =>
      setServerErr(e instanceof ApiError ? e.message : "Couldn't change the URL."),
  });

  const unchanged = slug === service.slug;
  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Change URL · ${service.name}`}
      description="Pick the path segment this service is reached under."
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={unchanged || slug === "" || slugError(slug) !== null || save.isPending}
            onClick={() => save.mutate()}
          >
            {save.isPending ? "Changing…" : "Change URL"}
          </Button>
        </>
      }
    >
      <ErrorNotice variant="warn" role="note">
        The old URL stops working immediately. Clients that use it must be updated.
      </ErrorNotice>
      <SlugField
        id="edit-slug"
        value={slug}
        onChange={(v) => { setSlug(v); setServerErr(null); }}
        error={serverErr}
      />
    </Dialog>
  );
}
