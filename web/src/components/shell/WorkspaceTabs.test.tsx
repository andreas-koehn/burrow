import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderToStaticMarkup } from "react-dom/server";
import { createElement } from "react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { NAVIGATIONS, navigationFor, workspacesFor } from "@/lib/navigation";
import { workspaceFor } from "@/lib/workspace";
import { WorkspaceTabs } from "./WorkspaceTabs";

const admin = { isAdmin: true, hasAiGateway: true };
const plain = { isAdmin: false, hasAiGateway: false };

function Probe() {
  return <div data-testid="path">{useLocation().pathname}</div>;
}

function Harness({ ctx, collapsed }: { ctx: typeof admin; collapsed?: boolean }) {
  const { pathname } = useLocation();
  return <WorkspaceTabs current={navigationFor(workspaceFor(pathname), ctx)} workspaces={workspacesFor(ctx)} collapsed={collapsed} />;
}

function mount(path: string, ctx = admin, collapsed = false) {
  render(<MemoryRouter initialEntries={[path]}><Harness ctx={ctx} collapsed={collapsed} /><Probe /></MemoryRouter>);
}

const tabs = () => within(screen.getByRole("navigation", { name: "Workspace" })).getAllByRole("link");
/** The drawing of a workspace's own icon, to compare with the one a tab shows. */
const drawing = (ws: "services" | "gateway") =>
  new DOMParser().parseFromString(renderToStaticMarkup(createElement(NAVIGATIONS[ws].icon)), "text/html").querySelector("svg")!.innerHTML;

describe("WorkspaceTabs", () => {
  it("shows both workspaces as links with their own icons", () => {
    mount("/services");
    expect(tabs().map((a) => [a.textContent, a.getAttribute("href")])).toEqual([["Services", "/"], ["AI Gateway", "/gateway"]]);
    const [services, gateway] = tabs();
    expect(services.querySelector("svg")!.innerHTML).toBe(drawing("services"));
    expect(gateway.querySelector("svg")!.innerHTML).toBe(drawing("gateway"));
    expect(drawing("services")).not.toBe(drawing("gateway"));
    // The icon is decoration: the name is the label.
    for (const tab of tabs()) expect(tab.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    // Links, not tabs of a tablist: they navigate.
    expect(screen.queryByRole("tab")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("marks the current workspace", () => {
    mount("/gateway/providers");
    const current = tabs().filter((a) => a.getAttribute("aria-current") === "page");
    expect(current.map((a) => a.textContent)).toEqual(["AI Gateway"]);
    expect(current[0]).toHaveClass("is-active");
    expect(screen.getByRole("link", { name: "Services" })).not.toHaveAttribute("aria-current");
    expect(screen.getByRole("link", { name: "Services" })).not.toHaveClass("is-active");
  });

  it("a click on the other workspace opens its home and moves the mark", async () => {
    mount("/services");
    await userEvent.click(screen.getByRole("link", { name: "AI Gateway" }));
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway$/);
    expect(screen.getByRole("link", { name: "AI Gateway" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Services" })).not.toHaveAttribute("aria-current");
  });

  it("is reached with Tab and followed with Enter", async () => {
    mount("/gateway/models");
    await userEvent.tab();
    expect(screen.getByRole("link", { name: "Services" })).toHaveFocus();
    await userEvent.tab();
    expect(screen.getByRole("link", { name: "AI Gateway" })).toHaveFocus();
    await userEvent.tab({ shift: true });
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/$/);
  });

  it("with one workspace it is not a control", () => {
    mount("/services", plain);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(screen.getByText("Services")).toBeInTheDocument();
  });

  it("never shows the relay namespace", () => {
    mount("/services");
    expect(document.body.textContent).not.toMatch(/\/svc\/|\/ai\//);
  });

  it("collapsed: icons only, each still named", () => {
    mount("/services", admin, true);
    expect(tabs().map((a) => [a.textContent, a.getAttribute("aria-label"), a.getAttribute("title")])).toEqual([
      ["", "Services", "Services"],
      ["", "AI Gateway", "AI Gateway"],
    ]);
    expect(screen.getByRole("link", { name: "Services" })).toHaveAttribute("aria-current", "page");
    for (const tab of tabs()) expect(tab.querySelector("svg")).not.toBeNull();
  });

  it("collapsed with one workspace: the mark alone, no name to cut off", () => {
    mount("/services", plain, true);
    expect(screen.queryByText("Services")).toBeNull();
    expect(document.querySelector("svg")).not.toBeNull();
  });

  it("in Settings neither is current and both still lead out", () => {
    mount("/settings/email");
    expect(tabs().map((a) => [a.textContent, a.getAttribute("href"), a.getAttribute("aria-current")])).toEqual([
      ["Services", "/", null],
      ["AI Gateway", "/gateway", null],
    ]);
  });
});
