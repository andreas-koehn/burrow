import { describe, it, expect } from "vitest";
import { serviceUrl, servicePath } from "./serviceUrl";

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
});
