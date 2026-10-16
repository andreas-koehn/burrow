import type { GuardrailDirection } from "@/lib/contract";

/** The relay's limit on custom guardrail patterns (internal/guardrails/custom.go). */
export const MAX_CUSTOM_PATTERNS = 50;

export const DIRECTION_LABEL: Record<GuardrailDirection, string> = {
  request: "Requests",
  answer: "Answers",
  both: "Requests and answers",
};
