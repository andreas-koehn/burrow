import { cx } from "./cx";

export interface ComboboxOption {
  /** What the input takes when the option is picked. */
  value: string;
  /** A readable name, shown next to the value. */
  label?: string;
  /** A short remark after the label. */
  hint?: string;
}

export interface ComboboxProps {
  id: string;
  value: string;
  onChange: (value: string) => void;
  options: ComboboxOption[];
  placeholder?: string;
  mono?: boolean;
  invalid?: boolean;
  "aria-describedby"?: string;
  /** Shown under the input while there is nothing to suggest. */
  emptyText?: string;
  maxLength?: number;
  required?: boolean;
  autoCapitalize?: string;
}

/**
 * A text input with suggestions: anything can be typed, the options only save
 * the typing. It is a native input with a datalist, so the keyboard and
 * screen-reader behaviour are the browser's.
 */
export function Combobox({
  id, value, onChange, options, placeholder, mono, invalid, "aria-describedby": describedBy, emptyText,
  maxLength, required, autoCapitalize = "none",
}: ComboboxProps) {
  const listId = `${id}-options`;
  const emptyId = `${id}-empty`;
  const empty = options.length === 0 && emptyText !== undefined;
  return (
    <>
      <input
        id={id}
        list={listId}
        className={cx("input", mono && "mono", invalid && "is-invalid")}
        value={value}
        placeholder={placeholder}
        maxLength={maxLength}
        required={required}
        autoComplete="off"
        autoCapitalize={autoCapitalize}
        spellCheck={false}
        aria-invalid={invalid || undefined}
        aria-describedby={[describedBy, empty ? emptyId : ""].filter(Boolean).join(" ") || undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      <datalist id={listId}>
        {options.map((o) => (
          <option key={o.value} value={o.value} label={[o.label, o.hint].filter(Boolean).join(" · ") || undefined} />
        ))}
      </datalist>
      {empty && <span id={emptyId} className="muted small">{emptyText}</span>}
    </>
  );
}
