import { FormField } from "@/components/ds";
import { MAX_SYSTEM_TEXT_BYTES, byteLength } from "@/lib/systemText";

export interface SystemTextFieldProps {
  /** Id of the textarea; its counter, help and error derive from it. */
  id: string;
  value: string;
  onChange: (next: string) => void;
  help: string;
}

/** A textarea for a system text, with a byte counter and the limit's message. */
export function SystemTextField({ id, value, onChange, help }: SystemTextFieldProps) {
  const bytes = byteLength(value);
  const tooLong = bytes > MAX_SYSTEM_TEXT_BYTES;
  return (
    <FormField
      label="System text"
      htmlFor={id}
      descId={`${id}-error`}
      error={tooLong ? `Too long: at most ${MAX_SYSTEM_TEXT_BYTES} bytes.` : undefined}
    >
      <textarea
        id={id}
        className="input resizable"
        rows={4}
        value={value}
        aria-invalid={tooLong || undefined}
        aria-describedby={`${id}-count ${id}-help${tooLong ? ` ${id}-error` : ""}`}
        onChange={(e) => onChange(e.target.value)}
      />
      <span id={`${id}-count`} className="muted small" aria-live="polite">{bytes} / {MAX_SYSTEM_TEXT_BYTES} bytes</span>
      <span id={`${id}-help`} className="help">{help}</span>
    </FormField>
  );
}
