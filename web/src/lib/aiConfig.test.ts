import { describe, it, expect } from "vitest";
import { DEFAULT_AI_CONFIG, withAIConfigDefaults } from "@/lib/aiConfig";

describe("withAIConfigDefaults", () => {
  // The relay captures a request only when the service's config switches the
  // inspector on (internal/aigw/chain.go, captureEntry): a service without one
  // captures nothing, and the dashboard must not say otherwise.
  it("a service without a stored config captures no requests", () => {
    expect(withAIConfigDefaults({}).inspector.enabled).toBe(false);
    expect(withAIConfigDefaults(null as unknown as object).inspector.enabled).toBe(false);
    expect(DEFAULT_AI_CONFIG.inspector.enabled).toBe(false);
  });

  it("keeps what the relay stored", () => {
    expect(withAIConfigDefaults({ inspector: { enabled: true, max_requests: 20 } }).inspector).toEqual({ enabled: true, max_requests: 20 });
  });
});
