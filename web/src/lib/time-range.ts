/** The periods a log view can look back over. */
export type TimeRange = "15m" | "1h" | "24h" | "7d";

export const TIME_RANGES: { value: TimeRange; label: string; ms: number }[] = [
  { value: "15m", label: "15 min", ms: 15 * 60_000 },
  { value: "1h", label: "1 hour", ms: 3_600_000 },
  { value: "24h", label: "24 hours", ms: 86_400_000 },
  { value: "7d", label: "7 days", ms: 7 * 86_400_000 },
];

/** The range named in a URL, or the fallback when the value is missing or unknown. */
export function parseTimeRange(value: string | null, fallback: TimeRange = "24h"): TimeRange {
  return TIME_RANGES.find((r) => r.value === value)?.value ?? fallback;
}

/** How far back a range reaches, in milliseconds. */
export function timeRangeMs(range: TimeRange): number {
  return TIME_RANGES.find((r) => r.value === range)!.ms;
}
