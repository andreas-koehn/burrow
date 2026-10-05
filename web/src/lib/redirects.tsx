import { Navigate, generatePath, useLocation, useParams } from "react-router-dom";

/**
 * Redirects an old path to its new home, keeping route params, the query
 * string and the hash. `to` may contain route params and its own query.
 */
export function RedirectTo({ to }: { to: string }) {
  const params = useParams();
  const { search, hash } = useLocation();
  // A "?" that ends a segment marks an optional param; any other one starts the query.
  const at = to.search(/\?(?!\/|$)/);
  const pattern = at < 0 ? to : to.slice(0, at);
  const own = at < 0 ? "" : to.slice(at + 1);
  const merged = new URLSearchParams(own);
  new URLSearchParams(search).forEach((v, k) => { if (!merged.has(k)) merged.append(k, v); });
  const query = merged.toString();
  return <Navigate replace to={{ pathname: generatePath(pattern, params), search: query ? `?${query}` : "", hash }} />;
}
