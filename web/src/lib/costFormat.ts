/** Currency with two decimals; a cost above zero that would round to $0.00 reads "<$0.01". */
export function fmtUsd(n: number): string {
  if (n > 0 && n.toFixed(2) === "0.00") return "<$0.01";
  return `$${n.toFixed(2)}`;
}

/** First 8 characters of an id: tells keys apart when the name is not known. */
export function shortId(id: string): string {
  return id.length > 8 ? `${id.slice(0, 8)}…` : id;
}

/** Token and request counts with thousands separators. */
export function fmtCount(n: number): string {
  return n.toLocaleString("en-US");
}
