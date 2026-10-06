import { describe, it, expect } from "vitest";
import { detectOs, shortcutLabel } from "./platform";

describe("shortcutLabel", () => {
  it.each(["MacIntel", "iPhone", "iPad", "Mac OS X"])("uses ⌘ on %s", (p) => {
    expect(shortcutLabel("K", p)).toBe("⌘K");
  });
  it.each(["Linux x86_64", "Win32", ""])("uses Ctrl on %j", (p) => {
    expect(shortcutLabel("K", p)).toBe("Ctrl K");
  });
});

describe("detectOs", () => {
  it.each([
    ["Win32", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "windows"],
    ["", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "windows"],
    ["MacIntel", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "macos"],
    ["", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "macos"],
    ["Linux x86_64", "Mozilla/5.0 (X11; Linux x86_64)", "linux"],
    ["", "", "linux"],
  ] as const)("platform %j, user agent %j → %s", (platform, ua, os) => {
    expect(detectOs(platform, ua)).toBe(os);
  });

  it("does not take the 'win' in Darwin for Windows", () => {
    expect(detectOs("", "curl/8 (Darwin)")).toBe("linux");
  });
});
