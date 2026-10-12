import { useId, type ReactNode } from "react";
import { cx } from "./cx";

export interface AccessModeCardProps {
  /** The radio's accessible name: the one or two words that say which choice this is. */
  title: ReactNode;
  /** What the choice does; read after the name, as its description. */
  description: ReactNode;
  selected: boolean;
  onSelect: () => void;
  className?: string;
}

export function AccessModeCard({
  title,
  description,
  selected,
  onSelect,
  className,
}: AccessModeCardProps) {
  const id = useId();
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      aria-labelledby={`${id}-title`}
      aria-describedby={`${id}-desc`}
      onClick={onSelect}
      className={cx("access-mode-card", className)}
    >
      <p id={`${id}-title`} className="title">{title}</p>
      <p id={`${id}-desc`} className="desc">{description}</p>
    </button>
  );
}
