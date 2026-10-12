import { useSyncExternalStore } from "react";
import { formatRelativeTime, formatTimestamp } from "@/lib/format";

export interface RelativeTimeProps {
  /** An RFC 3339 timestamp; null while there is none. */
  value: string | null | undefined;
  /** What stands there without a time. */
  empty?: string;
  /**
   * For a record such as the audit log: the exact time is the text and how
   * long ago it was is on hover.
   */
  absolute?: boolean;
}

// One clock for every relative time on the page: an interval that starts with
// the first one shown and stops with the last. A tick only says that a minute
// has passed; each time is then measured against the clock itself, so one
// shown between two ticks is not measured against a minute that is over.
const TICK_MS = 60_000;
const listeners = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | undefined;
let ticks = 0;

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  if (timer === undefined) {
    timer = setInterval(() => {
      ticks++;
      for (const l of listeners) l();
    }, TICK_MS);
  }
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0 && timer !== undefined) {
      clearInterval(timer);
      timer = undefined;
    }
  };
}

const currentTick = () => ticks;

/** A time that is there: it follows the clock. */
function Moment({ value, absolute }: { value: string; absolute: boolean }) {
  // Read for the re-render alone: without it "just now" would stay for good.
  useSyncExternalStore(subscribe, currentTick, currentTick);
  const exact = formatTimestamp(value);
  const said = formatRelativeTime(value);
  return (
    <time dateTime={value} title={absolute ? said : exact}>
      {absolute ? exact : said}
    </time>
  );
}

/**
 * A moment in a table or a sentence: how long ago it was ("3 days ago"), with
 * the exact UTC time on hover. The one place that writes a relative time, and
 * it keeps it up to date by the minute.
 */
export function RelativeTime({ value, empty = "—", absolute = false }: RelativeTimeProps) {
  if (!value || isNaN(new Date(value).getTime())) return <>{empty}</>;
  return <Moment value={value} absolute={absolute} />;
}
