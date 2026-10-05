import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input, Select } from "@/components/ds";
import { SlugField } from "@/components/SlugField";
import { providerSlugError } from "@/lib/providerSlug";
import { providerBaseUrl } from "@/lib/serviceUrl";
import type { AiProvider, Service } from "@/lib/contract";

export interface NewProviderDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/** Registers an existing http service in API-key mode as a model provider. */
export function NewProviderDialog({ open, onOpenChange }: NewProviderDialogProps) {
  // Mounted per opening, so every opening starts from an empty form.
  return open ? <NewProviderForm onOpenChange={onOpenChange} /> : null;
}

function NewProviderForm({ onOpenChange }: Pick<NewProviderDialogProps, "onOpenChange">) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [serviceId, setServiceId] = useState("");
  // slugErr belongs to the slug field; formErr is everything else (name,
  // service taken or not eligible, permission, network).
  const [slugErr, setSlugErr] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);

  const services = useQuery({
    queryKey: ["services"],
    queryFn: () => apiFetch<Service[]>("/services"),
    retry: false,
  });
  const providers = useQuery({
    queryKey: ["ai", "providers"],
    queryFn: () => apiFetch<AiProvider[]>("/ai/providers"),
    retry: false,
  });

  const taken = new Set((providers.data ?? []).map((p) => p.service_id));
  const eligible = (services.data ?? []).filter(
    (s) => s.type === "http" && s.access_mode === "api_key" && !taken.has(s.id),
  );
  const loaded = services.data !== undefined && providers.data !== undefined;
  const loadFailed = services.isError || providers.isError;
  // With a single candidate there is nothing to choose.
  const chosen = serviceId || (eligible.length === 1 ? eligible[0]!.id : "");

  const create = useMutation({
    mutationFn: () =>
      apiFetch<AiProvider>("/ai/providers", {
        method: "POST",
        body: JSON.stringify({ slug: slug || undefined, name: name.trim(), kind: "tunnel", service_id: chosen }),
      }),
    // Wait for the refetch so the list behind the dialog already shows the provider.
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ["ai", "providers"] });
      onOpenChange(false);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setFormErr("Couldn't create the provider.");
      // Only the slug rule ("slug must be …") is about the slug.
      else if (e.status === 400 && e.message.startsWith("slug")) setSlugErr(e.message);
      else if (e.status === 403) setFormErr("You don't have permission to add providers.");
      else setFormErr(e.message);
    },
  });

  const slugMessage = slugErr ?? providerSlugError(slug);
  const canCreate = name.trim() !== "" && chosen !== "" && slugMessage === null && !create.isPending;
  return (
    <Dialog
      open
      onOpenChange={onOpenChange}
      title="New provider"
      description="Serve a service in API-key mode as a model provider under its own base URL."
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="primary" disabled={!canCreate} onClick={() => create.mutate()}>
            {create.isPending ? "Creating…" : "Create"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField label="Name" htmlFor="np-name" w="md">
          <Input
            id="np-name"
            value={name}
            maxLength={120}
            required
            autoComplete="off"
            onChange={(e) => { setName(e.target.value); setFormErr(null); }}
          />
        </FormField>
        <SlugField
          id="np-slug"
          label="Provider slug"
          value={slug}
          onChange={(v) => { setSlug(v); setSlugErr(null); setFormErr(null); }}
          error={slugMessage}
          preview={(s) => providerBaseUrl(s)}
        />
        {eligible.length > 0 && (
          <FormField label="Service" htmlFor="np-service" w="md">
            <Select
              id="np-service"
              value={chosen}
              onChange={(v) => { setServiceId(v); setFormErr(null); }}
              options={eligible.map((s) => ({ value: s.id, label: s.name || s.id }))}
              placeholder="Select a service…"
            />
          </FormField>
        )}
      </FormFieldGroup>
      <p className="muted small">Leave the slug empty to derive it from the name.</p>
      {loadFailed ? (
        <ErrorNotice>Couldn't load the services to choose from.</ErrorNotice>
      ) : loaded && eligible.length === 0 ? (
        <ErrorNotice variant="info" role="status">
          No eligible service.{" "}
          <Link to="/services?new=ai">Create a service in API-key mode first.</Link>
        </ErrorNotice>
      ) : null}
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}
