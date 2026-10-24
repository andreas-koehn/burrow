/** The canonical gRPC status codes, by number. */
const GRPC_STATUS = [
  "OK", "CANCELLED", "UNKNOWN", "INVALID_ARGUMENT", "DEADLINE_EXCEEDED", "NOT_FOUND", "ALREADY_EXISTS",
  "PERMISSION_DENIED", "RESOURCE_EXHAUSTED", "FAILED_PRECONDITION", "ABORTED", "OUT_OF_RANGE",
  "UNIMPLEMENTED", "INTERNAL", "UNAVAILABLE", "DATA_LOSS", "UNAUTHENTICATED",
] as const;

/** The name of a grpc-status; "" for a number the spec does not define. */
export function grpcStatusName(code: number): string {
  return GRPC_STATUS[code] ?? "";
}

/** A grpc-status as the dashboard shows it: "14 UNAVAILABLE"; "" when the row has none. */
export function grpcStatusText(code: number | null | undefined): string {
  if (code === null || code === undefined) return "";
  const name = grpcStatusName(code);
  return name ? `${code} ${name}` : String(code);
}

/** The address a gRPC client dials for a custom domain: the relay's HTTPS port. "" for a wildcard. */
export function grpcAddress(hostname: string): string {
  return hostname.startsWith("*") ? "" : `${hostname}:443`;
}
