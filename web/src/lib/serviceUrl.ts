/** Path part of a service's public URL. */
export function servicePath(slug: string): string {
  return `/svc/${slug}/`;
}

/**
 * Full public URL of an http service. The relay reports it when it knows its
 * own domain; otherwise the dashboard's origin is the same origin by design.
 */
export function serviceUrl(slug: string, apiUrl?: string): string {
  if (apiUrl) return apiUrl;
  if (!slug) return "";
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  return `${origin}${servicePath(slug)}`;
}

/** Base URL an OpenAI-compatible client is configured with for a provider. */
export function providerBaseUrl(slug: string, apiUrl?: string): string {
  if (apiUrl) return apiUrl;
  const origin = typeof window !== "undefined" ? window.location.origin : "";
  return `${origin}/ai/${slug}/v1`;
}

/** Path of a full URL; the raw string when it does not parse as an absolute URL. */
export function urlPath(url: string): string {
  try {
    return new URL(url).pathname;
  } catch {
    return url;
  }
}
