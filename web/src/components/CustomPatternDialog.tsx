import { useState } from "react";
import { Button, Dialog, FormField, FormFieldGroup, Input, Select, SubmitHint } from "@/components/ds";
import { ApiError } from "@/lib/api";
import type { GuardrailCustomPattern, GuardrailDirection } from "@/lib/contract";

const MAX_PATTERN_LEN = 512;
const MAX_DESCRIPTION_LEN = 200;
const ID_RE = /^[a-z0-9_-]{1,64}$/;

const DIRECTION_OPTIONS = [
  { value: "request", label: "Requests" },
  { value: "answer", label: "Answers" },
  { value: "both", label: "Both" },
];

type Field = "id" | "pattern" | "direction" | "description";

/**
 * The field a relay message is about. Its messages read `custom pattern "<id>": the id …`,
 * `the pattern …`, `not a valid regular expression …`, `the description …`; anything else
 * (the count, a failed save) belongs to the dialog.
 */
function fieldOfMessage(message: string): Field | null {
  const rest = message.replace(/^custom pattern "[^"]*":\s*/, "");
  if (rest === message) return null;
  if (/^the id\b/.test(rest)) return "id";
  if (/^(the pattern\b|not a valid regular expression)/.test(rest)) return "pattern";
  if (/^the direction\b/.test(rest)) return "direction";
  if (/^the description\b/.test(rest)) return "description";
  return null;
}

export interface CustomPatternDialogProps {
  /** The pattern being edited; absent for a new one. */
  pattern?: GuardrailCustomPattern;
  /** Every id in use: the other custom patterns and the bundled ones. */
  takenIds: readonly string[];
  /** Stores the whole new pattern; rejects with the relay's ApiError. */
  onSave: (next: GuardrailCustomPattern) => Promise<void>;
  onClose: () => void;
}

/** Mounted while open, so each opening starts from a clean form. */
export function CustomPatternDialog({ pattern, takenIds, onSave, onClose }: CustomPatternDialogProps) {
  const editing = pattern !== undefined;
  const [id, setId] = useState(pattern?.id ?? "");
  const [regex, setRegex] = useState(pattern?.pattern ?? "");
  const [direction, setDirection] = useState<GuardrailDirection>(pattern?.direction ?? "request");
  const [description, setDescription] = useState(pattern?.description ?? "");
  const [left, setLeft] = useState<ReadonlySet<Field>>(new Set());
  const [serverErr, setServerErr] = useState<{ field: Field | null; message: string } | null>(null);
  const [pending, setPending] = useState(false);
  // A late answer to a dialog that was closed is dropped.
  const [closed, setClosed] = useState(false);

  const leave = (f: Field) => setLeft((s) => new Set(s).add(f));

  const idErr =
    id === "" ? "Enter an id."
    : !ID_RE.test(id) ? "Lowercase letters, digits, - and _, up to 64 characters."
    : !editing && takenIds.includes(id) ? "A pattern with this id already exists."
    : null;
  const patternErr =
    regex === "" ? "Enter a pattern."
    : [...regex].length > MAX_PATTERN_LEN ? `Up to ${MAX_PATTERN_LEN} characters.`
    : null;
  const descErr = [...description].length > MAX_DESCRIPTION_LEN ? `Up to ${MAX_DESCRIPTION_LEN} characters.` : null;

  const onServer = (f: Field) => (serverErr?.field === f ? serverErr.message : null);
  const shown = (f: Field, err: string | null) => onServer(f) ?? (left.has(f) ? err : null);

  const idShown = shown("id", idErr);
  const patternShown = shown("pattern", patternErr);
  const descShown = shown("description", descErr);

  const missing = [idErr, patternErr, descErr];
  const blocked = idErr !== null || patternErr !== null || descErr !== null || pending;

  function close() {
    setClosed(true);
    onClose();
  }

  async function submit() {
    if (blocked) return;
    setServerErr(null);
    setPending(true);
    try {
      await onSave({ id, pattern: regex, direction, description });
    } catch (e) {
      if (closed) return;
      const message = e instanceof ApiError ? e.message : "Couldn't save the pattern.";
      setServerErr({ field: fieldOfMessage(message), message });
      setPending(false);
    }
  }

  // An edit clears the relay's answer about that field.
  const edited = (f: Field) => { if (serverErr?.field === f) setServerErr(null); };

  return (
    <Dialog
      open
      onOpenChange={(o) => { if (!o) close(); }}
      title={editing ? `Edit ${pattern.id}` : "New custom pattern"}
      description="Matched against the text of requests, answers or both. The id is what the log and the webhook name."
      size="md"
      footer={
        <>
          <SubmitHint id="custom-pattern-hint" missing={missing} />
          <Button variant="secondary" onClick={close}>Cancel</Button>
          <Button
            variant="primary"
            disabled={blocked}
            aria-describedby={blocked && !pending ? "custom-pattern-hint" : undefined}
            onClick={submit}
          >
            {pending ? "Saving…" : editing ? "Save changes" : "Add"}
          </Button>
        </>
      }
    >
      <form onSubmit={(e) => { e.preventDefault(); void submit(); }}>
        <FormFieldGroup>
          <FormField
            label="ID"
            htmlFor="cp-id"
            descId="cp-id-desc"
            help="Lowercase letters, digits, - and _."
            error={idShown}
          >
            <Input
              id="cp-id"
              className="mono"
              spellCheck={false}
              disabled={editing}
              invalid={idShown !== null}
              aria-describedby="cp-id-desc"
              value={id}
              onChange={(e) => { setId(e.target.value); edited("id"); }}
              onBlur={() => leave("id")}
            />
          </FormField>
          <FormField
            label="Pattern"
            htmlFor="cp-pattern"
            descId="cp-pattern-desc"
            help="A regular expression in Go RE2 syntax, up to 512 characters."
            error={patternShown}
          >
            <Input
              id="cp-pattern"
              className="mono"
              spellCheck={false}
              invalid={patternShown !== null}
              aria-describedby="cp-pattern-desc"
              value={regex}
              onChange={(e) => { setRegex(e.target.value); edited("pattern"); }}
              onBlur={() => leave("pattern")}
            />
          </FormField>
          <FormField label="Direction" htmlFor="cp-direction" error={onServer("direction")} descId="cp-direction-desc">
            <Select
              id="cp-direction"
              aria-describedby={onServer("direction") ? "cp-direction-desc" : undefined}
              options={DIRECTION_OPTIONS}
              value={direction}
              onChange={(v) => { setDirection(v as GuardrailDirection); edited("direction"); }}
            />
          </FormField>
          <FormField
            label="Description"
            htmlFor="cp-description"
            descId="cp-description-desc"
            help="Optional. Shown in this list."
            error={descShown}
          >
            <Input
              id="cp-description"
              invalid={descShown !== null}
              aria-describedby="cp-description-desc"
              value={description}
              onChange={(e) => { setDescription(e.target.value); edited("description"); }}
              onBlur={() => leave("description")}
            />
          </FormField>
        </FormFieldGroup>
        {serverErr && serverErr.field === null && <p role="alert" className="notice-inline error">{serverErr.message}</p>}
        <button type="submit" hidden />
      </form>
    </Dialog>
  );
}
