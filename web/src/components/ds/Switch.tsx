export interface SwitchProps {
  checked: boolean;
  onChange?: (checked: boolean) => void;
  id?: string;
  "aria-label"?: string;
  "aria-describedby"?: string;
  "aria-disabled"?: boolean | "true" | "false";
  disabled?: boolean;
  title?: string;
}

export function Switch({
  checked,
  onChange,
  id,
  "aria-label": ariaLabel,
  "aria-describedby": ariaDescribedBy,
  "aria-disabled": ariaDisabled,
  disabled,
  title,
}: SwitchProps) {
  const isDisabled = disabled || ariaDisabled === true || ariaDisabled === "true";
  return (
    <button
      type="button"
      id={id}
      role="switch"
      aria-checked={checked}
      aria-label={ariaLabel}
      aria-describedby={ariaDescribedBy}
      aria-disabled={isDisabled || undefined}
      disabled={disabled}
      title={title}
      className="switch"
      onClick={() => {
        if (!isDisabled) onChange?.(!checked);
      }}
    />
  );
}
