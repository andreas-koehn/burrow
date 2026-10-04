import { describe, it, expect } from "vitest";
import { shortcutLabel } from "./platform";

describe("shortcutLabel", () => {
  it.each(["MacIntel", "iPhone", "iPad", "Mac OS X"])("uses ⌘ on %s", (p) => {
    expect(shortcutLabel("K", p)).toBe("⌘K");
  });
  it.each(["Linux x86_64", "Win32", ""])("uses Ctrl on %j", (p) => {
    expect(shortcutLabel("K", p)).toBe("Ctrl K");
  });
});
