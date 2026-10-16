/** The first sentence of a description; a summary without a full stop ends at its paragraph. */
export function firstSentence(text: string): string {
  const para = text.trim().split(/\n\s*\n/)[0].replace(/\s+/g, " ");
  const m = para.match(/^.*?[.!?](?=\s|$)/);
  return (m ? m[0] : para).trim();
}
