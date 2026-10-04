import { FormField, Input } from "@/components/ds";
import { serviceUrl } from "@/lib/serviceUrl";

export const SLUG_RE = /^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$/;
export const SLUG_HINT =
  "3–40 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen.";

/** Local validation message for a slug, or null when it is fine or empty. */
export function slugError(value: string): string | null {
  if (value === "" || SLUG_RE.test(value)) return null;
  return SLUG_HINT;
}

export interface SlugFieldProps {
  id: string;
  value: string;
  onChange: (value: string) => void;
  /** Error reported by the server (e.g. "slug already in use"). */
  error?: string | null;
}

export function SlugField({ id, value, onChange, error }: SlugFieldProps) {
  const message = error ?? slugError(value);
  const errId = `${id}-err`;
  const previewId = `${id}-preview`;
  const preview = value && !message ? serviceUrl(value) : "";
  return (
    <FormField
      label="URL slug"
      htmlFor={id}
      w="md"
      error={message ? <span id={errId}>{message}</span> : undefined}
      help={preview ? <span id={previewId} className="mono">{preview}</span> : undefined}
    >
      <Input
        id={id}
        mono
        value={value}
        invalid={!!message}
        aria-describedby={message ? errId : preview ? previewId : undefined}
        onChange={(e) => onChange(e.target.value.toLowerCase())}
      />
    </FormField>
  );
}
