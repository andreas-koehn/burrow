import { useId, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { apiFetch, ApiError } from "@/lib/api";
import { Button, Dialog, Disclosure, ErrorNotice, FormField, FormFieldGroup, Input, SubmitHint, firstMissing } from "@/components/ds";
import { toolSlugError, toolSlugFromName } from "@/lib/toolSource";
import type { ToolSource, ToolSourceCreate } from "@/lib/contract";

export interface NewRemoteToolSourceDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/**
 * Adds a tool server that the relay calls itself: its address, and the name of
 * the slot on the relay that holds its credential. The credential itself is
 * never typed here; it is set on the relay.
 */
export function NewRemoteToolSourceDialog({ open, onOpenChange }: NewRemoteToolSourceDialogProps) {
  // Mounted per opening, so every opening starts from an empty form.
  return open ? <Form onOpenChange={onOpenChange} /> : null;
}

const SLOT = /^[A-Z0-9_]{1,32}$/;

function wholeNumber(text: string, max: number): number | null | "bad" {
  if (text.trim() === "") return null;
  const n = Number(text);
  return Number.isInteger(n) && n >= 0 && n <= max ? n : "bad";
}

function Form({ onOpenChange }: Pick<NewRemoteToolSourceDialogProps, "onOpenChange">) {
  const qc = useQueryClient();
  const nav = useNavigate();
  const reasonId = useId();
  const [name, setName] = useState("");
  const [slug, setSlug] = useState("");
  const [slugEdited, setSlugEdited] = useState(false);
  const [url, setUrl] = useState("");
  const [slot, setSlot] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const [header, setHeader] = useState("");
  const [format, setFormat] = useState("");
  const [revision, setRevision] = useState("");
  const [rpm, setRpm] = useState("");
  const [concurrent, setConcurrent] = useState("");
  const [timeout, setTimeoutS] = useState("");
  const [left, setLeft] = useState<Record<string, boolean>>({});
  const [slugErr, setSlugErr] = useState<string | null>(null);
  const [formErr, setFormErr] = useState<string | null>(null);

  const leave = (k: string) => setLeft((l) => ({ ...l, [k]: true }));
  const slugProblem = toolSlugError(slug);
  const urlProblem = !/^https:\/\/[^/\s?#@]+\S*$/.test(url.trim()) ? "The address must start with https://." : null;
  const slotProblem = slot !== "" && !SLOT.test(slot) ? "Use 1 to 32 capital letters, digits or _." : null;
  const formatProblem = format !== "" && format.split("{key}").length !== 2 ? "Put {key} in it once." : null;
  const rpmV = wholeNumber(rpm, 100000);
  const concV = wholeNumber(concurrent, 1000);
  const timeoutV = wholeNumber(timeout, 300);
  const numberProblem = rpmV === "bad" || concV === "bad" || timeoutV === "bad" || timeoutV === 0
    ? "Calls per minute, calls at once and the timeout are whole numbers in range." : null;

  const missing = firstMissing([
    slug === "" && "Enter a name.",
    slugProblem && slug !== "" && "Choose another name.",
    url.trim() === "" && "Enter the address.",
    urlProblem && url.trim() !== "" && "Fix the address.",
    slotProblem && "Fix the credential slot.",
    formatProblem && "Fix the format.",
    numberProblem && "Fix the numbers.",
  ]);

  const create = useMutation({
    mutationFn: () => {
      const body: ToolSourceCreate = { slug, name: name.trim() || slug, kind: "remote", url: url.trim() };
      if (slot !== "") body.credential_slot = slot;
      if (header.trim() !== "") body.auth_header = header.trim();
      if (format !== "") body.auth_format = format;
      if (revision.trim() !== "") body.protocol_version = revision.trim();
      if (typeof rpmV === "number") body.rpm = rpmV;
      if (typeof concV === "number") body.max_concurrent = concV;
      if (typeof timeoutV === "number") body.timeout_s = timeoutV;
      return apiFetch<ToolSource>("/mcp/sources", { method: "POST", body: JSON.stringify(body) });
    },
    onSuccess: async (made) => {
      await qc.invalidateQueries({ queryKey: ["mcp", "sources"] });
      onOpenChange(false);
      nav(`/gateway/tools/${made?.slug ?? slug}`);
    },
    onError: (e: unknown) => {
      if (!(e instanceof ApiError)) setFormErr("Couldn't add the source.");
      else if (e.status === 409) setSlugErr(`${e.message}. Choose another name.`);
      else if (e.status === 403) setFormErr("Adding a tool source needs the permission to manage tool sources.");
      else setFormErr(e.message);
    },
  });

  function typeName(v: string) {
    setName(v);
    if (!slugEdited) setSlug(toolSlugFromName(v));
    setSlugErr(null); setFormErr(null);
  }
  const slugMessage = slugErr ?? (slugProblem && (left.slug || left.name) && slug !== "" ? slugProblem : null);

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!create.isPending) onOpenChange(o); }}
      title="Add remote source"
      description="A tool server on the internet that the relay calls itself."
      footer={
        <>
          <SubmitHint id={reasonId} missing={[missing]} />
          <Button variant="secondary" disabled={create.isPending} onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button
            variant="primary"
            disabled={missing !== undefined || create.isPending}
            aria-describedby={missing ? reasonId : undefined}
            onClick={() => { setFormErr(null); setSlugErr(null); create.mutate(); }}
          >
            {create.isPending ? "Adding…" : "Add source"}
          </Button>
        </>
      }
    >
      <FormFieldGroup>
        <FormField label="Name" htmlFor="rs-name" w="md">
          <Input id="rs-name" value={name} maxLength={120} autoComplete="off" onChange={(e) => typeName(e.target.value)} onBlur={() => leave("name")} />
        </FormField>
        <FormField
          label="Slug"
          htmlFor="rs-slug"
          w="md"
          descId="rs-slug-desc"
          error={slugMessage ?? undefined}
          help={slugProblem === null ? <>Served at <span className="mono">/mcp/{slug}</span>; its tools are named <span className="mono">{slug}__&lt;tool&gt;</span> on <span className="mono">/mcp</span>.</> : "Made from the name; 1 to 24 letters, digits or hyphens."}
        >
          <Input
            id="rs-slug"
            mono
            value={slug}
            maxLength={24}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            invalid={slugMessage !== null}
            aria-describedby="rs-slug-desc"
            onChange={(e) => { setSlug(e.target.value); setSlugEdited(true); setSlugErr(null); setFormErr(null); }}
            onBlur={() => leave("slug")}
          />
        </FormField>
        <FormField
          label="URL"
          htmlFor="rs-url"
          descId="rs-url-desc"
          error={left.url && url.trim() !== "" && urlProblem ? urlProblem : undefined}
          help="The MCP address of the server, for example https://mcp.example.com/mcp."
        >
          <Input
            id="rs-url"
            mono
            value={url}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            invalid={Boolean(left.url && url.trim() !== "" && urlProblem)}
            aria-describedby="rs-url-desc"
            onChange={(e) => { setUrl(e.target.value); setFormErr(null); }}
            onBlur={() => leave("url")}
          />
        </FormField>
        <FormField
          label="Credential slot"
          htmlFor="rs-slot"
          w="md"
          descId="rs-slot-desc"
          error={slotProblem ?? undefined}
          help={<>Leave empty when the server needs none. The value is set on the relay as <span className="mono">BURROW_UPSTREAM_KEY_{slot || "<SLOT>"}</span>; it is never typed here.</>}
        >
          <Input
            id="rs-slot"
            mono
            value={slot}
            maxLength={32}
            autoComplete="off"
            autoCapitalize="characters"
            spellCheck={false}
            invalid={slotProblem !== null}
            aria-describedby="rs-slot-desc"
            onChange={(e) => { setSlot(e.target.value.toUpperCase()); setFormErr(null); }}
          />
        </FormField>
      </FormFieldGroup>
      <Disclosure label="Advanced" open={advanced} onOpenChange={setAdvanced}>
        <FormFieldGroup>
          <FormField label="Header" htmlFor="rs-header" w="md" help="Where the credential is sent. Authorization when left empty.">
            <Input id="rs-header" mono value={header} autoComplete="off" spellCheck={false} onChange={(e) => setHeader(e.target.value)} />
          </FormField>
          <FormField label="Format" htmlFor="rs-format" w="md" descId="rs-format-desc" error={formatProblem ?? undefined} help="How it is written, with {key} for the credential. Bearer {key} when left empty.">
            <Input id="rs-format" mono value={format} autoComplete="off" spellCheck={false} invalid={formatProblem !== null} aria-describedby="rs-format-desc" onChange={(e) => setFormat(e.target.value)} />
          </FormField>
          <FormField label="Revision" htmlFor="rs-revision" w="md" help="An MCP revision such as 2025-06-18 to pin toward the server. Empty negotiates.">
            <Input id="rs-revision" mono value={revision} autoComplete="off" spellCheck={false} onChange={(e) => setRevision(e.target.value)} />
          </FormField>
          <FormField label="Calls per minute" htmlFor="rs-rpm" w="sm" help="0 or empty: no limit.">
            <Input id="rs-rpm" inputMode="numeric" value={rpm} onChange={(e) => setRpm(e.target.value)} />
          </FormField>
          <FormField label="Calls at once" htmlFor="rs-conc" w="sm" help="0 or empty: no limit.">
            <Input id="rs-conc" inputMode="numeric" value={concurrent} onChange={(e) => setConcurrent(e.target.value)} />
          </FormField>
          <FormField label="Timeout (seconds)" htmlFor="rs-timeout" w="sm" descId="rs-timeout-desc" error={numberProblem ?? undefined} help="60 when left empty; at most 300.">
            <Input id="rs-timeout" inputMode="numeric" value={timeout} invalid={numberProblem !== null} aria-describedby="rs-timeout-desc" onChange={(e) => setTimeoutS(e.target.value)} />
          </FormField>
        </FormFieldGroup>
      </Disclosure>
      {formErr && <ErrorNotice>{formErr}</ErrorNotice>}
    </Dialog>
  );
}
