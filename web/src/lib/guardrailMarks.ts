import type { InspectorEntry } from "@/lib/contract";

/** One thing the guardrails did to a request, in words. */
export interface GuardrailMarkView {
  kind: "guardrail" | "judge" | "judge-call";
  /** The words shown in the list and in the detail header. */
  text: string;
  /** The judge's reason, when it gave one; shown as the title in the list and in full in the detail. */
  reason?: string;
  /** A sentence for the inspector that the list marker has no room for. */
  detail?: string;
  /** What was inspected: the request or its answer. */
  direction?: "request" | "answer";
}

const FAILURE: Record<string, string> = {
  timeout: "timeout", error: "error", malformed: "malformed", unavailable: "unavailable", too_large: "too large",
};

/** The marks of a captured request: pattern hit, judge outcome, and whether it is the relay's own judge call. */
export function guardrailMarks(e: Pick<InspectorEntry, "guardrail" | "judge" | "purpose">): GuardrailMarkView[] {
  const out: GuardrailMarkView[] = [];
  const g = e.guardrail;
  if (g) {
    // A request is flagged by log_only and refused by the other actions; an answer by flag and block.
    const blocked = g.action !== "log_only" && g.action !== "flag";
    const word = !blocked ? "Flagged" : g.mid_stream ? "Blocked mid-answer" : "Blocked";
    const fromKey = g.in === "key_system_text";
    out.push({
      kind: "guardrail",
      text: `${word} · ${g.pattern}${fromKey ? " (key system text)" : ""}`,
      detail: fromKey ? "Matched the gateway key's system text." : undefined,
      direction: g.direction,
    });
  }
  const j = e.judge;
  if (j) {
    let text: string;
    let detail: string | undefined;
    switch (j.verdict) {
      case "allow": text = "Judged · allowed"; break;
      case "block": text = "Judged · refused"; break;
      case "not_judged": text = `Not judged · ${j.reason || "no text"}`; break;
      default: {
        const cls = FAILURE[j.failure ?? ""] ?? j.failure ?? "error";
        text = `Judge failed · ${j.refused ? "refused" : cls}`;
        detail = j.refused
          ? `The judge failed (${cls}), so the request was refused.`
          : `The judge failed (${cls}); the request was let through.`;
      }
    }
    out.push({ kind: "judge", text, detail, reason: j.verdict === "block" ? j.reason : undefined, direction: j.direction });
  }
  if (e.purpose === "guardrail-judge") out.push({ kind: "judge-call", text: "Judge call" });
  return out;
}

const BINARY = /^<binary, (\d+) bytes>$/;

/** What the inspector shows for a body the relay did not store: it keeps only "<binary, N bytes>". */
export function binaryBodyNote(body: string): string | null {
  const m = BINARY.exec(body);
  return m ? `Binary body, ${m[1]} bytes, not stored.` : null;
}
