import { describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { RedirectTo } from "./redirects";
import { OLD_ROUTES, NOT_YET_MOVED } from "./moved-routes";

function Landing() {
  const { pathname, search, hash } = useLocation();
  return <div data-testid="landing">{pathname + search + hash}</div>;
}

function landingFor(visit: string, routes: { from: string; to: string }[] = OLD_ROUTES) {
  cleanup();
  render(
    <MemoryRouter initialEntries={[visit]}>
      <Routes>
        {routes.map((r) => <Route key={r.from} path={r.from} element={<RedirectTo to={r.to} />} />)}
        <Route path="*" element={<Landing />} />
      </Routes>
    </MemoryRouter>,
  );
  return screen.getByTestId("landing").textContent;
}

describe("RedirectTo", () => {
  it.each([
    ["/cache", "/gateway/cache"],
    ["/guardrails#custom", "/gateway/guardrails#custom"],
    ["/cost?window=week", "/gateway/cost?window=week"],
    ["/inspector", "/gateway/requests"],
    ["/inspector/svc1", "/gateway/requests/svc1"],
    ["/inspector/svc1/req9?tab=diff#body", "/gateway/requests/svc1/req9?tab=diff#body"],
    ["/inspector/my%20svc", "/gateway/requests/my%20svc"],
  ])("%s lands on %s", (from, to) => {
    expect(landingFor(from)).toBe(to);
  });

  it("merges the target's own query with the incoming one", () => {
    expect(landingFor("/tunnels?q=web", [{ from: "/tunnels", to: "/services?live=1" }])).toBe("/services?live=1&q=web");
  });

  it("lets the target's own query win over an incoming duplicate", () => {
    expect(landingFor("/tunnels?live=0&q=web", [{ from: "/tunnels", to: "/services?live=1" }])).toBe("/services?live=1&q=web");
  });
});

describe("route tables", () => {
  it("no old path is also a new one, and no dashboard path uses a relay prefix", () => {
    const targets = new Set(OLD_ROUTES.map((r) => r.to));
    for (const r of OLD_ROUTES) {
      expect(targets.has(r.from)).toBe(false);
      expect(r.to).not.toMatch(/^\/(ai|svc|openai|anthropic|api|__burrow)(\/|$)/);
    }
  });

  it("a page that has not moved yet is reached from its future path, query and hash intact", () => {
    expect(landingFor("/settings/users?q=a#invite", NOT_YET_MOVED)).toBe("/users?q=a#invite");
    expect(landingFor("/traffic", NOT_YET_MOVED)).toBe("/connection-logs");
  });

  it("the two tables never chain into a loop", () => {
    const old = new Set(OLD_ROUTES.map((r) => r.from));
    for (const r of NOT_YET_MOVED) expect(old.has(r.to)).toBe(false);
  });
});
