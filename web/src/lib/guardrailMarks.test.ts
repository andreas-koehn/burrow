import { describe, it, expect } from "vitest";
import { guardrailMarks } from "./guardrailMarks";

describe("guardrailMarks", () => {
  it("a judge failure that refused the request says so", () => {
    const [m] = guardrailMarks({ judge: { direction: "request", verdict: "failed", failure: "timeout", refused: true } });
    expect(m!.text).toBe("Judge failed · refused");
    expect(m!.detail).toMatch(/timeout/);
    expect(m!.detail).toMatch(/refused/i);
  });

  it("a judge failure without refused names the class and says the request went through", () => {
    const [m] = guardrailMarks({ judge: { direction: "request", verdict: "failed", failure: "too_large" } });
    expect(m!.text).toBe("Judge failed · too large");
    expect(m!.detail).toMatch(/too large/);
    expect(m!.detail).toMatch(/let through/i);
  });

  it("a refusal on the key's system text says so in the marker and the detail", () => {
    const [m] = guardrailMarks({ guardrail: { pattern: "ignore-previous", direction: "request", action: "refuse_403", in: "key_system_text" } });
    expect(m!.text).toBe("Blocked · ignore-previous (key system text)");
    expect(m!.detail).toBe("Matched the gateway key's system text.");
  });

  it("a match in the request itself has no source note", () => {
    const [m] = guardrailMarks({ guardrail: { pattern: "ignore-previous", direction: "request", action: "refuse_403" } });
    expect(m!.text).toBe("Blocked · ignore-previous");
    expect(m!.detail).toBeUndefined();
  });
});
