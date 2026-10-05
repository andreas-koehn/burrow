import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, ErrorNotice, FormField, FormFieldGroup, Input } from "@/components/ds";
import { SlugField } from "@/components/SlugField";
import { providerSlugError } from "@/lib/providerSlug";
import { providerBaseUrl } from "@/lib/serviceUrl";
import type { AiProvider } from "@/lib/contract";

export interface RenameProviderDialogProps {
  provider: Pick<AiProvider, "slug" | "name">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called after a successful save, once the lists are fresh. */
  onRenamed: (next: AiProvider) => void;
}

export function RenameProviderDialog({ open, ...rest }: RenameProviderDialogProps) {
  // Mounted per opening, so every opening starts from the current name and slug.
  return open ? <RenameProviderForm {...rest} /> : null;
}

function RenameProviderForm({ provider, onOpenChange, onRenamed }: Omit<RenameProviderDialogProps, "open">) {
  const qc = useQueryClient();
  const [name, setName] = useState(provider.name);
  const [slug, setSlug] = useState(provider.slug);
  // Same split as EditSlugDialog: field errors on their field, the rest in the body.
  const [nameErr, setNameErr] = useState<string | null>(null);
  const [slugErr, setSlugErr] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: () =>
      apiFetch<AiProvider>(`/ai/providers/${provider.slug}`, {
        method: "PUT",
        body: JSON.stringify({ slug, name: name.trim() }),
      }),
    onSuccess: async (next) => {
      // Seed the entry under the new slug so the page behind the dialog shows
      // the new name and URL at once, and wait for the list to be fresh.
      qc.setQueryData(["ai", "provider", next.slug], next);
      await qc.invalidateQueries({ queryKey: ["ai", "providers"] });
      onOpenChange(false);
      onRenamed(next);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setFormErr("Couldn't rename the provider.");
      else if (e.status === 400 && e.message.startsWith("name")) setNameErr(e.message);
      // On a rename the only conflict is the slug.
      else if (e.status === 400 || e.status === 409) setSlugErr(e.message);
      else if (e.status === 403) setFormErr("You don't have permission to rename providers.");
      else setFormErr(e.message);
    },
  });

  const slugMessage = slugErr ?? providerSlugError(slug);
  const slugChanged = slug !== provider.slug;
  const unchanged = !slugChanged && name.trim() === provider.name;
  return (
    <Dialog
      open
      onOpenChange={onOpenChange}
      title={`Rename provider · ${provider.name}`}
      description="Change the display name and the slug in the base URL."
      footer={
        <>
          <Button variant="secondary" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={unchanged || name.trim() === "" || slug === "" || slugMessage !== null || save.isPending}
            onClick={() => save.mutate()}
          >
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <ErrorNotice variant="warn" role="note">
        The old base URL stops working immediately. Clients that use it must be updated.
      </ErrorNotice>
      <FormFieldGroup>
        <FormField
          label="Name"
          htmlFor="rp-name"
          w="md"
          error={nameErr ? <span id="rp-name-err">{nameErr}</span> : undefined}
        >
          <Input
            id="rp-name"
            value={name}
            maxLength={120}
            required
            autoComplete="off"
            invalid={!!nameErr}
            aria-describedby={nameErr ? "rp-name-err" : undefined}
            onChange={(e) => { setName(e.target.value); setNameErr(null); setFormErr(null); }}
          />
        </FormField>
        <SlugField
          id="rp-slug"
          label="Provider slug"
          value={slug}
          onChange={(v) => { setSlug(v); setSlugErr(null); setFormErr(null); }}
          error={slugMessage}
          preview={(s) => providerBaseUrl(s)}
        />
      </FormFieldGroup>
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}
