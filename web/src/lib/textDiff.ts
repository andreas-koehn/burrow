export interface DiffPart {
  kind: "same" | "del" | "add";
  text: string;
}

// Words and the white space between them, so that joining the parts gives the text back.
function pieces(s: string): string[] {
  return s.match(/\s+|[^\s]+/g) ?? [];
}

/**
 * A word-level difference between two texts, for reading what an upstream
 * changed in a tool description. The parts of kind same and del, joined, are
 * the old text; those of kind same and add, joined, are the new one.
 * Very long texts fall back to one removed and one added part: the
 * comparison is quadratic.
 */
export function diffWords(oldText: string, newText: string): DiffPart[] {
  const a = pieces(oldText);
  const b = pieces(newText);
  if (a.length * b.length > 4_000_000) {
    return [
      ...(oldText ? [{ kind: "del" as const, text: oldText }] : []),
      ...(newText ? [{ kind: "add" as const, text: newText }] : []),
    ];
  }
  // Longest common subsequence, filled from the end.
  const lcs: number[][] = Array.from({ length: a.length + 1 }, () => new Array<number>(b.length + 1).fill(0));
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lcs[i]![j] = a[i] === b[j] ? lcs[i + 1]![j + 1]! + 1 : Math.max(lcs[i + 1]![j]!, lcs[i]![j + 1]!);
    }
  }
  const out: DiffPart[] = [];
  const push = (kind: DiffPart["kind"], text: string) => {
    const last = out[out.length - 1];
    if (last && last.kind === kind) last.text += text;
    else out.push({ kind, text });
  };
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) { push("same", a[i]!); i++; j++; }
    else if (lcs[i + 1]![j]! >= lcs[i]![j + 1]!) { push("del", a[i]!); i++; }
    else { push("add", b[j]!); j++; }
  }
  while (i < a.length) push("del", a[i++]!);
  while (j < b.length) push("add", b[j++]!);
  return out;
}
