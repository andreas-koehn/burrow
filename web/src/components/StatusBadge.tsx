import { cx } from "@/components/ds";
import { STATE_BADGE, STATE_LABEL, type State } from "@/lib/status";

/**
 * The status of a service, a provider or a client: a dot and the word. The
 * word is always there, so the colour never carries the meaning alone.
 */
export function StatusBadge({ state, className }: { state: State; className?: string }) {
  return (
    <span className={cx("badge", STATE_BADGE[state], className)}>
      <span className="dot" aria-hidden="true" />
      {STATE_LABEL[state]}
    </span>
  );
}
