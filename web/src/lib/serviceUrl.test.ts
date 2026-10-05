import { describe, it, expect } from "vitest";
import { providerBaseUrl, serviceUrl, servicePath, urlPath } from "./serviceUrl";

describe("serviceUrl", () => {
  it("prefers the URL reported by the API", () => {
    expect(serviceUrl("p7baeh", "https://burrow.example.com/svc/p7baeh/"))
      .toBe("https://burrow.example.com/svc/p7baeh/");
  });
  it("falls back to the dashboard origin when the API reports none", () => {
    expect(serviceUrl("p7baeh", "")).toBe(`${window.location.origin}/svc/p7baeh/`);
    expect(serviceUrl("p7baeh")).toBe(`${window.location.origin}/svc/p7baeh/`);
  });
  it("is empty without a slug", () => {
    expect(serviceUrl("", "")).toBe("");
  });
  it("servicePath is the path part", () => {
    expect(servicePath("my-app")).toBe("/svc/my-app/");
  });
  it("providerBaseUrl prefers the API value and falls back to the origin", () => {
    expect(providerBaseUrl("ollama", "https://b.example.com/ai/ollama/v1")).toBe("https://b.example.com/ai/ollama/v1");
    expect(providerBaseUrl("ollama", "")).toBe(`${window.location.origin}/ai/ollama/v1`);
  });
  it("urlPath extracts the pathname and tolerates malformed input", () => {
    expect(urlPath("https://burrow.example.com/svc/x/")).toBe("/svc/x/");
    expect(urlPath("not a url")).toBe("not a url");
    expect(urlPath("/svc/x/")).toBe("/svc/x/");
  });
});
