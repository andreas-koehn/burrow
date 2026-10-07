import type { ReactNode } from "react";
import { cx } from "./cx";

export type FormFieldWidth = "sm" | "md" | "lg" | "full";

export interface FormFieldProps {
  label: ReactNode;
  htmlFor: string;
  help?: ReactNode;
  error?: ReactNode;
  /** id of the help or error text, for aria-describedby on the control. */
  descId?: string;
  w?: FormFieldWidth;
  children: ReactNode;
  className?: string;
}

export function FormField({
  label,
  htmlFor,
  help,
  error,
  descId,
  w = "full",
  children,
  className,
}: FormFieldProps) {
  // Prefixed so it can never collide with a Tailwind width utility
  // (`.w-md` is `width: 28rem` in Tailwind v4 and blew fields out of dialogs).
  const widthClass = `field-w-${w}`;
  return (
    <div className={cx("form-field", widthClass, className)}>
      <label htmlFor={htmlFor}>{label}</label>
      {children}
      {error != null ? (
        <span id={descId} className="error" role="alert">{error}</span>
      ) : help != null ? (
        <span id={descId} className="help">{help}</span>
      ) : null}
    </div>
  );
}
