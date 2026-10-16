const ms = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });

/** A measured duration; a request captured before the relay measured it (0 or missing) shows a dash. */
export function Latency({ ms: value }: { ms?: number }) {
  if (!value || value <= 0) return <span title="Not measured">—</span>;
  return <>{ms.format(value)} ms</>;
}
