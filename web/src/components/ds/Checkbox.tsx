import { Check, Minus } from "lucide-react";

export interface CheckboxProps {
  checked?: boolean;
  onChange?: (checked: boolean) => void;
  id?: string;
  indeterminate?: boolean;
  /** Id of the element that describes the checkbox (help text). */
  describedBy?: string;
}

export function Checkbox({ checked, onChange, id, indeterminate, describedBy }: CheckboxProps) {
  const state = indeterminate ? "mixed" : checked ? "true" : "false";
  return (
    <button
      type="button"
      id={id}
      role="checkbox"
      aria-checked={state}
      aria-describedby={describedBy}
      className="checkbox"
      onClick={() => onChange?.(!checked)}
    >
      {indeterminate ? (
        <Minus size={11} strokeWidth={2.5} />
      ) : checked ? (
        <Check size={11} strokeWidth={2.5} />
      ) : null}
    </button>
  );
}
