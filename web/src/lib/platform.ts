/** Keyboard shortcut hint for the current platform: ⌘K on Apple, Ctrl K elsewhere. */
export function shortcutLabel(
  key: string,
  platform: string = typeof navigator !== "undefined" ? navigator.platform : "",
): string {
  return /mac|iphone|ipad|ipod/i.test(platform) ? `⌘${key}` : `Ctrl ${key}`;
}

/** The operating systems the burrow client is built for, as the dashboard names them. */
export type ClientOs = "linux" | "macos" | "windows";

/** The visitor's operating system, as far as the browser tells; Linux when it does not. */
export function detectOs(
  platform: string = typeof navigator !== "undefined" ? navigator.platform : "",
  userAgent: string = typeof navigator !== "undefined" ? navigator.userAgent : "",
): ClientOs {
  const s = `${platform} ${userAgent}`;
  // Not a bare "win": Darwin contains it.
  if (/windows|win32|win64/i.test(s)) return "windows";
  if (/mac|iphone|ipad|ipod/i.test(s)) return "macos";
  return "linux";
}
