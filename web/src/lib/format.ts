/**
 * Format a byte count using IEC binary prefixes (KiB, MiB, GiB, …).
 * Returns "0 B" for 0, NaN, or non-finite values.
 */
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "0 B";
  if (n === 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  const i = Math.min(Math.floor(Math.log2(n) / 10), units.length - 1);
  const value = n / Math.pow(1024, i);
  // 1 decimal place for anything above plain bytes
  return i === 0 ? `${n} B` : `${value.toFixed(1)} ${units[i]}`;
}

const TS_FORMAT = new Intl.DateTimeFormat("en-GB", {
  day: "2-digit",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hour12: false,
  timeZone: "UTC",
  timeZoneName: "short",
});

/**
 * Format an RFC3339 timestamp into an unambiguous human-readable string
 * (en-GB locale, UTC zone). Returns "—" for null/undefined/empty/unparseable.
 *
 * Always UTC so tail-tracing across distributed components stays
 * timezone-neutral.
 */
export function formatTimestamp(s: string | null | undefined): string {
  if (!s) return "—";
  const d = new Date(s);
  if (isNaN(d.getTime())) return "—";
  return TS_FORMAT.format(d);
}

const DATE_FORMAT = new Intl.DateTimeFormat("en-GB", {
  day: "2-digit",
  month: "short",
  year: "numeric",
  timeZone: "UTC",
});

const MINUTE_MS = 60_000;
const HOUR_MS = 60 * MINUTE_MS;
const DAY_MS = 24 * HOUR_MS;

function ago(n: number, unit: string): string {
  return `${n} ${unit}${n === 1 ? "" : "s"} ago`;
}

/**
 * How long ago a timestamp was: "just now" under a minute, then minutes,
 * hours and days, rounded down ("3 days ago"), and the UTC date for anything
 * older than 30 days. A time in the future is a clock that runs ahead, so it
 * is written out as formatTimestamp() writes it. Returns "—" for
 * null/undefined/empty/unparseable input.
 *
 * Called by components/RelativeTime only, which puts the exact time beside it.
 */
export function formatRelativeTime(s: string | null | undefined, now: number = Date.now()): string {
  if (!s) return "—";
  const d = new Date(s);
  if (isNaN(d.getTime())) return "—";
  const diffMs = now - d.getTime();
  if (diffMs < 0) return TS_FORMAT.format(d);
  if (diffMs < MINUTE_MS) return "just now";
  if (diffMs < HOUR_MS) return ago(Math.floor(diffMs / MINUTE_MS), "minute");
  if (diffMs < DAY_MS) return ago(Math.floor(diffMs / HOUR_MS), "hour");
  if (diffMs <= 30 * DAY_MS) return ago(Math.floor(diffMs / DAY_MS), "day");
  return DATE_FORMAT.format(d);
}

/** A latency in whole milliseconds; "—" when there is nothing to measure. */
export function formatLatency(ms: number, samples: number): string {
  if (!Number.isFinite(ms) || !(samples > 0)) return "—";
  return `${Math.round(ms).toLocaleString("en-US")} ms`;
}

/** A share in whole percent; "—" when the whole is nothing. A measured zero is "0%". */
export function formatRatio(part: number, whole: number): string {
  if (!Number.isFinite(part) || !Number.isFinite(whole) || !(whole > 0)) return "—";
  return `${Math.round((part / whole) * 100)}%`;
}
