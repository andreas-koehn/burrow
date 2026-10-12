import { firstMissing, type Missing } from "./firstMissing";

export interface SubmitHintProps {
  /** Named by the button's aria-describedby. */
  id: string;
  /** What keeps the button disabled, most important first; anything falsy is not missing. */
  missing: Missing[];
}

/**
 * Goes into a dialog footer, left of the buttons: the first thing that is
 * missing, or nothing. Linked to the button with aria-describedby.
 */
export function SubmitHint({ id, missing }: SubmitHintProps) {
  const first = firstMissing(missing);
  if (first === undefined) return null;
  // A status: what is missing changes while the reader fills in the form.
  return <span id={id} role="status" className="submit-hint muted small">{first}</span>;
}
