// Format translation as the dashboard words it. The gateway answers a request
// in a format a model has no target for by translating it through a pair
// (internal/aigw/translate); a pair id is "<caller format>-<target format>".

/** The pairs the relay has, in words: what the caller sends → what the provider is asked. */
const PAIRS: Record<string, string> = {
  "messages-chat": "Anthropic Messages → Chat Completions",
  "responses-chat": "OpenAI Responses → Chat Completions",
  "chat-messages": "Chat Completions → Anthropic Messages",
  "responses-messages": "OpenAI Responses → Anthropic Messages",
};

/** A pair id in words; an id this dashboard does not know is shown as it is. */
export function pairWords(pair: string): string {
  // An own key only: "constructor" is no pair.
  return Object.hasOwn(PAIRS, pair) ? PAIRS[pair]! : pair;
}

/** Whether any of the pairs asks an Anthropic-format provider. */
export function towardMessages(pairs: string[] | undefined): boolean {
  return (pairs ?? []).some((p) => p.endsWith("-messages"));
}

/** Several pairs in words, in the order they are tried. */
export function pairsWords(pairs: string[]): string {
  return pairs.map(pairWords).join(" or ");
}

/**
 * The names a translation left out. The relay ends a list it had to cut with
 * "more": that is a mark, not a name.
 */
export function droppedNames(dropped: string[] | null | undefined): { names: string[]; more: boolean } {
  const list = Array.isArray(dropped) ? dropped.filter((n) => typeof n === "string" && n !== "") : [];
  const more = list.length > 0 && list[list.length - 1] === "more";
  return { names: more ? list.slice(0, -1) : list, more };
}
