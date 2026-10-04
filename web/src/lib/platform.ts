/** Keyboard shortcut hint for the current platform: ⌘K on Apple, Ctrl K elsewhere. */
export function shortcutLabel(
  key: string,
  platform: string = typeof navigator !== "undefined" ? navigator.platform : "",
): string {
  return /mac|iphone|ipad|ipod/i.test(platform) ? `⌘${key}` : `Ctrl ${key}`;
}
