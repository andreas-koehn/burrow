import { describe, it, expect } from "vitest";
import { statusLabel } from "./status";

describe("statusLabel", () => {
  it.each([
    ["Connected", "connected"],
    ["Offline", "offline"],
    ["Degraded", "degraded"],
    ["closed_clean", "closed clean"],
    ["idle", "idle"],
    ["", ""],
  ])("%j → %j", (raw, want) => {
    expect(statusLabel(raw)).toBe(want);
  });
});
