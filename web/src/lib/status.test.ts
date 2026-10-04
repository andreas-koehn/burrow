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
    ["Some_New_State", "some new state"],
    [undefined, ""],
    [null, ""],
  ])("%j → %j", (raw, want) => {
    expect(statusLabel(raw as string)).toBe(want);
  });
});
