import { describe, it, expect } from "vitest";
import { formatBytes, formatRatio, formatRelativeTime, formatTimestamp } from "./format";

describe("formatBytes", () => {
  it("returns '0 B' for 0", () => {
    expect(formatBytes(0)).toBe("0 B");
  });
  it("returns '0 B' for NaN", () => {
    expect(formatBytes(NaN)).toBe("0 B");
  });
  it("returns '0 B' for negative", () => {
    expect(formatBytes(-1)).toBe("0 B");
  });
  it("formats bytes under 1024 as plain B", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(1)).toBe("1 B");
  });
  it("formats KiB correctly", () => {
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1536)).toBe("1.5 KiB");
  });
  it("formats MiB correctly", () => {
    expect(formatBytes(1024 * 1024)).toBe("1.0 MiB");
    expect(formatBytes(1024 * 1024 * 2.5)).toBe("2.5 MiB");
  });
  it("formats GiB correctly", () => {
    expect(formatBytes(1024 * 1024 * 1024)).toBe("1.0 GiB");
  });
  it("returns '0 B' for Infinity", () => {
    expect(formatBytes(Infinity)).toBe("0 B");
  });
});

describe("formatTimestamp", () => {
  it("returns '—' for null", () => {
    expect(formatTimestamp(null)).toBe("—");
  });
  it("returns '—' for undefined", () => {
    expect(formatTimestamp(undefined)).toBe("—");
  });
  it("returns '—' for empty string", () => {
    expect(formatTimestamp("")).toBe("—");
  });
  it("returns '—' for an invalid date string", () => {
    expect(formatTimestamp("not-a-date")).toBe("—");
  });
  it("returns a non-empty string for a valid RFC3339 timestamp", () => {
    const result = formatTimestamp("2024-01-15T10:30:00Z");
    expect(result).not.toBe("—");
    expect(result.length).toBeGreaterThan(0);
  });
  it("returns a non-empty string for another valid RFC3339 timestamp", () => {
    const result = formatTimestamp("2023-06-01T00:00:00.000Z");
    expect(result).not.toBe("—");
  });
});

describe("formatTimestamp locale", () => {
  it("renders en-GB unambiguous format regardless of system locale", () => {
    const s = "2026-05-25T07:42:51.834Z";
    const out = formatTimestamp(s);
    // Guarantee no month-first US ambiguity (no "5/25").
    expect(out).not.toMatch(/^\d{1,2}\/\d{1,2}\/\d{4}/);
    expect(out).not.toBe("—");
    expect(out).toContain("2026");
  });
});

describe("formatRatio", () => {
  it("no samples is a dash", () => {
    expect(formatRatio(0, 0)).toBe("—");
  });
  it("a measured zero is a zero", () => expect(formatRatio(0, 10)).toBe("0%"));
  it("rounds a share to whole percent", () => {
    expect(formatRatio(42, 100)).toBe("42%");
    expect(formatRatio(1, 3)).toBe("33%");
    expect(formatRatio(10, 10)).toBe("100%");
  });
  it("is a dash for a count or a figure that is no number", () => {
    expect(formatRatio(NaN, 10)).toBe("—");
    expect(formatRatio(1, NaN)).toBe("—");
  });
});

describe("formatRelativeTime", () => {
  const now = Date.parse("2026-10-06T19:46:56Z");
  const ago = (ms: number) => new Date(now - ms).toISOString();
  const MIN = 60_000, HOUR = 60 * MIN, DAY = 24 * HOUR;

  it("is 'just now' under a minute", () => {
    expect(formatRelativeTime(ago(0), now)).toBe("just now");
    expect(formatRelativeTime(ago(59_000), now)).toBe("just now");
  });
  it("counts minutes, hours and days, rounded down", () => {
    expect(formatRelativeTime(ago(MIN), now)).toBe("1 minute ago");
    expect(formatRelativeTime(ago(59 * MIN + 59_000), now)).toBe("59 minutes ago");
    expect(formatRelativeTime(ago(90 * MIN), now)).toBe("1 hour ago");
    expect(formatRelativeTime(ago(23 * HOUR), now)).toBe("23 hours ago");
    expect(formatRelativeTime(ago(3 * DAY), now)).toBe("3 days ago");
    expect(formatRelativeTime(ago(30 * DAY), now)).toBe("30 days ago");
  });
  it("is the date for anything older than 30 days", () => {
    // The short name of the month is the runtime's: "Sep" or "Sept".
    expect(formatRelativeTime(ago(30 * DAY + 1000), now)).toMatch(/^06 Sept? 2026$/);
    expect(formatRelativeTime("2025-01-02T03:04:05Z", now)).toBe("02 Jan 2025");
  });
  it("is the absolute time for a time in the future (clock skew)", () => {
    const soon = new Date(now + 5 * MIN).toISOString();
    expect(formatRelativeTime(soon, now)).toBe(formatTimestamp(soon));
  });
  it("returns '—' for nothing and for what is no time", () => {
    expect(formatRelativeTime(null, now)).toBe("—");
    expect(formatRelativeTime("", now)).toBe("—");
    expect(formatRelativeTime("not-a-date", now)).toBe("—");
  });
});
