import { describe, it, expect } from "vitest";
import { GLOSSARY } from "./glossary";

describe("GLOSSARY", () => {
  const requiredKeys = [
    "client",
    "tunnel",
    "service",
    "endpoint",
    "customDomain",
    "clientToken",
    "automationToken",
  ] as const;

  it("has all required keys", () => {
    for (const key of requiredKeys) {
      expect(GLOSSARY).toHaveProperty(key);
    }
  });

  it("every value is a non-empty string with no leading/trailing whitespace", () => {
    for (const key of requiredKeys) {
      const val = GLOSSARY[key];
      expect(typeof val).toBe("string");
      expect(val.length).toBeGreaterThan(0);
      expect(val).toBe(val.trim());
    }
  });
});
