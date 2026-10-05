import { useEffect } from "react";
import { describe, it, expect } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { useUrlParams } from "./use-url-params";

const api: { setParam: (name: string, value: string) => void; go: (to: string | number) => void } = {
  setParam: () => {},
  go: () => {},
};
const setParam = (name: string, value: string) => api.setParam(name, value);
const go = (to: string | number) => api.go(to);

function Probe() {
  const [params, set] = useUrlParams();
  const { pathname, search } = useLocation();
  const navigate = useNavigate();
  useEffect(() => {
    api.setParam = set;
    api.go = (to) => (typeof to === "number" ? navigate(to) : navigate(to));
  }, [set, navigate]);
  return (
    <>
      <div data-testid="shown">{params.toString()}</div>
      <div data-testid="address">{pathname + search}</div>
    </>
  );
}
const mount = (route: string) => render(<MemoryRouter initialEntries={["/before", route]}><Probe /></MemoryRouter>);
const shown = () => screen.getByTestId("shown").textContent;
const address = () => screen.getByTestId("address").textContent;

describe("useUrlParams", () => {
  it("reads the query string, sets a name and removes it with an empty value", () => {
    mount("/traffic?range=7d");
    expect(shown()).toBe("range=7d");
    act(() => setParam("q", "closed"));
    expect(shown()).toBe("range=7d&q=closed");
    expect(address()).toBe("/traffic?range=7d&q=closed");
    act(() => setParam("range", ""));
    expect(shown()).toBe("q=closed");
    expect(address()).toBe("/traffic?q=closed");
  });

  it("two changes in the same tick add up instead of the second dropping the first", () => {
    mount("/traffic");
    act(() => { setParam("range", "7d"); setParam("kind", "control"); });
    expect(shown()).toBe("range=7d&kind=control");
    expect(address()).toBe("/traffic?range=7d&kind=control");
  });

  it("replaces the address: back leaves the page instead of undoing a filter", () => {
    mount("/traffic");
    act(() => setParam("q", "a"));
    act(() => setParam("q", "ab"));
    act(() => go(-1));
    expect(address()).toBe("/before");
  });

  it("follows an address that changed elsewhere, and builds on it", () => {
    mount("/traffic");
    act(() => setParam("q", "a"));
    act(() => go("/traffic?service=svc1"));
    expect(shown()).toBe("service=svc1");
    act(() => setParam("range", "1h"));
    expect(shown()).toBe("service=svc1&range=1h");
    expect(address()).toBe("/traffic?service=svc1&range=1h");
  });

  it("set and unset again in one tick leaves nothing outstanding", () => {
    mount("/traffic");
    act(() => { setParam("q", "a"); setParam("q", ""); });
    expect(shown()).toBe("");
    act(() => go("/traffic?q=a"));
    expect(shown()).toBe("q=a");
  });
});
