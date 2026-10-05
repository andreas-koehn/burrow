import { useEffect, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice } from "@/components/ds";
import { SlugField, slugError } from "@/components/SlugField";
import type { Service } from "@/lib/contract";

// The one 409 that is about the slug; the other (tcp service) is not.
const SLUG_TAKEN = "slug already in use";

export interface EditSlugDialogProps {
  service: Pick<Service, "id" | "name" | "slug">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function EditSlugDialog({ service, open, onOpenChange }: EditSlugDialogProps) {
  const qc = useQueryClient();
  const [slug, setSlug] = useState(service.slug);
  // slugErr belongs to the slug itself (invalid, taken) and shows on the field;
  // formErr is everything else (permission, wrong service type, network).
  const [slugErr, setSlugErr] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);

  useEffect(() => {
    if (open) { setSlug(service.slug); setSlugErr(null); setFormErr(null); }
  }, [open, service.slug]);

  const save = useMutation({
    mutationFn: () =>
      apiFetch<{ slug: string; url: string }>(`/services/${service.id}/slug`, {
        method: "PUT",
        body: JSON.stringify({ slug }),
      }),
    // Wait for the refetches so the page behind the dialog never shows the old URL.
    onSuccess: async () => {
      await Promise.all([
        qc.invalidateQueries({ queryKey: ["services"] }),
        qc.invalidateQueries({ queryKey: ["service", service.id] }),
        qc.invalidateQueries({ queryKey: ["tunnels"] }),
      ]);
      onOpenChange(false);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setFormErr("Couldn't change the URL.");
      else if (e.status === 400 || (e.status === 409 && e.message === SLUG_TAKEN)) setSlugErr(e.message);
      else if (e.status === 403) setFormErr("You don't have permission to change this service's URL.");
      else setFormErr(e.message);
    },
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
        onChange={(v) => { setSlug(v); setSlugErr(null); setFormErr(null); }}
        error={slugErr}
      />
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}
