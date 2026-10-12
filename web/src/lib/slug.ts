/** The relay's rule for a service id (serviceIDRe in internal/api/service_handlers.go). */
const SERVICE_ID_RE = /^[a-z0-9_-]{3,64}$/;
export const SERVICE_ID_HINT = "3–64 characters: lowercase letters, digits, hyphens and underscores.";

/** Local validation message for a service id, or null when the relay takes it. */
export function serviceIdError(value: string): string | null {
  return SERVICE_ID_RE.test(value) ? null : SERVICE_ID_HINT;
}

/** Cuts to `max` characters and takes off what the cut left hanging at the end. */
function cut(value: string, max: number, edge: RegExp): string {
  return value.length > max ? value.slice(0, max).replace(edge, "") : value;
}

/**
 * A service id from a name: lower case, every run of characters an id cannot
 * hold one hyphen, a letter or a digit at both ends, 64 characters at most.
 * Shorter than three characters, the relay does not take it (see
 * serviceIdError); empty when the name has no letter or digit.
 */
export function deriveServiceId(name: string): string {
  // The id is what a client passes with --name: it must not begin like a flag.
  const id = name.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^[-_]+|[-_]+$/g, "");
  return cut(id, 64, /[-_]+$/);
}

/**
 * A URL slug from a name, by the relay's slug rule
 * (^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$): lower case, every run of other
 * characters one hyphen, none at either end, 40 characters at most.
 */
export function deriveSlug(name: string): string {
  const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "");
  return cut(slug, 40, /-+$/);
}
