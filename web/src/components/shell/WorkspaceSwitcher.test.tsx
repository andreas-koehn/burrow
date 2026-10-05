import { describe, it, expect } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, useLocation } from "react-router-dom";
import { navigationFor, workspacesFor } from "@/lib/navigation";
import { workspaceFor } from "@/lib/workspace";
import { WorkspaceSwitcher } from "./WorkspaceSwitcher";

const admin = { isAdmin: true, hasAiGateway: true };
const plain = { isAdmin: false, hasAiGateway: false };

function Probe() {
  return <div data-testid="path">{useLocation().pathname}</div>;
}

function Harness({ ctx }: { ctx: typeof admin }) {
  const { pathname } = useLocation();
  return <WorkspaceSwitcher current={navigationFor(workspaceFor(pathname), ctx)} workspaces={workspacesFor(ctx)} />;
}

function mount(path: string, ctx = admin) {
  render(<MemoryRouter initialEntries={[path]}><Harness ctx={ctx} /><Probe /></MemoryRouter>);
}

describe("WorkspaceSwitcher", () => {
  it("is a menu button that names the current workspace and shows its namespace", () => {
    mount("/services");
    const button = screen.getByRole("button", { name: "Workspace: Services" });
    expect(button).toHaveAttribute("aria-haspopup", "menu");
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(button).toHaveTextContent("Services");
    expect(button).toHaveTextContent("/svc/…");
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("lists both workspaces as radio items with the current one checked", async () => {
    mount("/services");
    await userEvent.click(screen.getByRole("button", { name: "Workspace: Services" }));
    const items = within(screen.getByRole("menu", { name: "Workspace" })).getAllByRole("menuitemradio");
    expect(items.map((i) => [i.textContent, i.getAttribute("aria-checked")])).toEqual([
      ["Services/svc/…", "true"],
      ["AI Gateway/ai/…", "false"],
    ]);
    expect(items[0]).toHaveFocus();
  });

  it("choosing the other workspace navigates to its home and closes the menu", async () => {
    mount("/services");
    await userEvent.click(screen.getByRole("button", { name: "Workspace: Services" }));
    await userEvent.click(screen.getByRole("menuitemradio", { name: /AI Gateway/ }));
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway$/);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByRole("button", { name: "Workspace: AI Gateway" })).toHaveTextContent("/ai/…");
  });

  it("works from the keyboard: arrows, Home/End, Enter", async () => {
    mount("/services");
    const button = screen.getByRole("button", { name: "Workspace: Services" });
    button.focus();
    await userEvent.keyboard("{ArrowDown}");
    const [services, gateway] = screen.getAllByRole("menuitemradio");
    expect(services).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(gateway).toHaveFocus();
    await userEvent.keyboard("{ArrowDown}");
    expect(services).toHaveFocus();
    await userEvent.keyboard("{End}");
    expect(gateway).toHaveFocus();
    await userEvent.keyboard("{Home}");
    expect(services).toHaveFocus();
    await userEvent.keyboard("{ArrowUp}");
    expect(gateway).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/gateway$/);
  });

  it("Space chooses the focused workspace", async () => {
    mount("/gateway/cost");
    await userEvent.click(screen.getByRole("button", { name: "Workspace: AI Gateway" }));
    expect(screen.getByRole("menuitemradio", { name: /AI Gateway/ })).toHaveFocus();
    await userEvent.keyboard("{Home}");
    await userEvent.keyboard(" ");
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/$/);
  });

  it("Escape closes the menu and returns focus to the button", async () => {
    mount("/services");
    const button = screen.getByRole("button", { name: "Workspace: Services" });
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(button).toHaveFocus();
    expect(screen.getByTestId("path")).toHaveTextContent(/^\/services$/);
  });

  // jsdom has no sequential focus navigation of its own, so this pins the two halves the
  // browser needs: focus is back on the button, and Tab keeps its default action.
  // web/e2e/workspaces.spec.ts checks in Chrome that the focus then lands on the next control.
  it("Tab closes the menu, puts focus back on the button and is not swallowed", async () => {
    mount("/services");
    const button = screen.getByRole("button", { name: "Workspace: Services" });
    await userEvent.click(button);
    const item = screen.getAllByRole("menuitemradio")[0];
    expect(item).toHaveFocus();
    const notPrevented = fireEvent.keyDown(item, { key: "Tab" });
    expect(notPrevented).toBe(true);
    expect(screen.queryByRole("menu")).toBeNull();
    expect(button).toHaveFocus();
  });

  it("marks the current workspace with a check, not with the hover background", async () => {
    mount("/gateway");
    await userEvent.click(screen.getByRole("button", { name: "Workspace: AI Gateway" }));
    const [services, gateway] = screen.getAllByRole("menuitemradio");
    expect(gateway.querySelector(".menu-check svg")).not.toBeNull();
    expect(services.querySelector(".menu-check svg")).toBeNull();
    expect(gateway).not.toHaveClass("is-focus");
    expect(gateway).toHaveTextContent(/^AI Gateway\/ai\/…$/);
  });

  it("a click outside closes the menu", async () => {
    mount("/services");
    await userEvent.click(screen.getByRole("button", { name: "Workspace: Services" }));
    await userEvent.click(screen.getByTestId("path"));
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("with one workspace it is a plain label, not a button", () => {
    mount("/services", plain);
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByText("Services")).toBeInTheDocument();
    expect(screen.getByText("/svc/…")).toBeInTheDocument();
  });

  it("names the AI Gateway on a gateway path", () => {
    mount("/gateway/providers");
    expect(screen.getByRole("button", { name: "Workspace: AI Gateway" })).toHaveTextContent("/ai/…");
  });

  it("collapsed, it keeps its name while the label is not rendered", () => {
    render(
      <MemoryRouter>
        <WorkspaceSwitcher current={navigationFor("services", admin)} workspaces={workspacesFor(admin)} collapsed />
      </MemoryRouter>,
    );
    expect(screen.getByRole("button", { name: "Workspace: Services" })).not.toHaveTextContent("Services");
  });
});
