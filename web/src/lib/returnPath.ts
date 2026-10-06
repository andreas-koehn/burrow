/**
 * Where to go after signing in. RequireAuth hands the address the visitor
 * asked for to the login page as router state `{ from }`; this reads it back.
 * Only a path of this dashboard counts: it starts with one slash, holds no
 * backslash and no control character, and is not the login page itself.
 * Everything else leads to the start page.
 */
export function returnPathFrom(state: unknown): string {
  if (typeof state !== "object" || state === null) return "/";
  const from = (state as { from?: unknown }).from;
  if (typeof from !== "string" || from.length > 2048) return "/";
  if (!from.startsWith("/") || from.startsWith("//")) return "/";
  // eslint-disable-next-line no-control-regex -- control characters are exactly what is refused here
  if (/[\\\u0000-\u001f\u007f]/.test(from)) return "/";
  if (/^\/login(?:[/?#]|$)/i.test(from)) return "/";
  return from;
}
