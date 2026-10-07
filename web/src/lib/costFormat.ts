/** Currency as the cost page has always shown it: two decimals. */
export function fmtUsd(n: number): string {
  return `$${n.toFixed(2)}`;
}

/** Token and request counts with thousands separators. */
export function fmtCount(n: number): string {
  return n.toLocaleString("en-US");
}
