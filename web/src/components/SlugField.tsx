import { useState, type ReactNode } from "react";
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
  label?: string;
  /** URL the slug results in, shown under a valid value. */
  preview?: (slug: string) => string;
  /** Shown under the field while it is empty. */
  help?: ReactNode;
  /** The local rule, when it is stricter than the slug rule (a provider slug has a reserved word). */
  rule?: (value: string) => string | null;
  /** The rule is named only once the field was left; a server error shows at once. */
  errorAfterBlur?: boolean;
  onBlur?: () => void;
}

export function SlugField({ id, value, onChange, error, label = "URL slug", preview: previewOf = serviceUrl, help, rule = slugError, errorAfterBlur = false, onBlur }: SlugFieldProps) {
  const [left, setLeft] = useState(false);
  const broken = rule(value);
  const message = error ?? (errorAfterBlur && !left ? null : broken);
  const errId = `${id}-err`;
  const previewId = `${id}-preview`;
  const preview = value && !message && !broken ? previewOf(value) : "";
  const helpId = `${id}-help`;
  const shownHelp = !message && !preview && value === "" ? help : undefined;
  return (
    <FormField
      label={label}
      htmlFor={id}
      w="md"
      error={message ? <span id={errId}>{message}</span> : undefined}
      help={preview ? <span id={previewId} className="mono">{preview}</span>
        : shownHelp != null ? <span id={helpId}>{shownHelp}</span> : undefined}
    >
      <Input
        id={id}
        mono
        value={value}
        invalid={!!message}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        aria-describedby={message ? errId : preview ? previewId : shownHelp != null ? helpId : undefined}
        onChange={(e) => onChange(e.target.value.toLowerCase())}
        onBlur={() => { setLeft(true); onBlur?.(); }}
      />
    </FormField>
  );
}
