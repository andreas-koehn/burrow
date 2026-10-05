/** The periods a log view can look back over; "all" has no lower bound. */
export type TimeRange = "15m" | "1h" | "24h" | "7d" | "all";

export const TIME_RANGES: { value: TimeRange; label: string; ms: number | null }[] = [
  { value: "15m", label: "15 min", ms: 15 * 60_000 },
  { value: "1h", label: "1 hour", ms: 3_600_000 },
  { value: "24h", label: "24 hours", ms: 86_400_000 },
  { value: "7d", label: "7 days", ms: 7 * 86_400_000 },
  { value: "all", label: "All", ms: null },
];

/** The range named in a URL, or the fallback when the value is missing or unknown. */
export function parseTimeRange(value: string | null, fallback: TimeRange = "24h"): TimeRange {
  return TIME_RANGES.find((r) => r.value === value)?.value ?? fallback;
}

/** How far back a range reaches, in milliseconds; null when it has no lower bound. */
export function timeRangeMs(range: TimeRange): number | null {
  return TIME_RANGES.find((r) => r.value === range)!.ms;
}

/** The next longer period to offer when a view is empty, if there is one. */
export function widerRange(range: TimeRange): { value: TimeRange; label: string } | undefined {
  if (range === "all") return undefined;
  return range === "7d" ? { value: "all", label: "Show all" } : { value: "7d", label: "Show the last 7 days" };
}
