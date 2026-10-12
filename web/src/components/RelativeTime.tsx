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

/**
 * A moment in a table or a sentence: how long ago it was ("3 days ago"), with
 * the exact UTC time on hover. The one place that writes a relative time.
 */
export function RelativeTime({ value, empty = "—", absolute = false }: RelativeTimeProps) {
  if (!value || isNaN(new Date(value).getTime())) return <>{empty}</>;
  const exact = formatTimestamp(value);
  const said = formatRelativeTime(value);
  return (
    <time dateTime={value} title={absolute ? said : exact}>
      {absolute ? exact : said}
    </time>
  );
}
