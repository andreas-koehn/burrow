import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { breadcrumbFor } from "@/lib/navigation";
import { TopBar } from "./TopBar";

const admin = { isAdmin: true, hasAiGateway: true };

function mount(path: string, props: { collapsed?: boolean; onToggle?: () => void } = {}) {
  const onToggle = props.onToggle ?? vi.fn();
  render(
    <MemoryRouter initialEntries={[path]}>
      <TopBar crumbs={breadcrumbFor(path, admin)} collapsed={props.collapsed ?? false} onToggle={onToggle} />
    </MemoryRouter>,
  );
  return onToggle;
}

describe("TopBar", () => {
  it("shows workspace, entry and object as a breadcrumb", () => {
    mount("/gateway/providers/zai");
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByRole("link", { name: "AI Gateway" })).toHaveAttribute("href", "/gateway");
    expect(within(crumbs).getByRole("link", { name: "Providers" })).toHaveAttribute("href", "/gateway/providers");
    const current = within(crumbs).getByText("zai");
    expect(current).toHaveAttribute("aria-current", "page");
    expect(current.tagName).not.toBe("A");
    expect(within(crumbs).getAllByRole("listitem")).toHaveLength(3);
  });

  it("hides the separators from assistive tech", () => {
    mount("/gateway/providers/zai");
    const seps = screen.getAllByText("/");
    expect(seps).toHaveLength(2);
    for (const s of seps) expect(s).toHaveAttribute("aria-hidden", "true");
  });

  it("links the service and marks the request on a request page", () => {
    mount("/gateway/requests/svc1/req9");
    const crumbs = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumbs).getByRole("link", { name: "svc1" })).toHaveAttribute("href", "/gateway/requests/svc1");
    expect(within(crumbs).getByText("req9")).toHaveAttribute("aria-current", "page");
  });

  it("does not mark a lone workspace crumb as the current page", () => {
    render(
      <MemoryRouter>
        <TopBar crumbs={[{ label: "Services", to: "/" }]} collapsed={false} onToggle={() => {}} />
      </MemoryRouter>,
    );
    expect(screen.getByRole("link", { name: "Services" })).not.toHaveAttribute("aria-current");
  });

  it("has a collapse toggle that reports its state", async () => {
    const onToggle = mount("/services");
    const button = screen.getByRole("button", { name: "Collapse sidebar" });
    expect(button).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(button);
    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it("offers to expand when collapsed", () => {
    mount("/services", { collapsed: true });
    expect(screen.getByRole("button", { name: "Expand sidebar" })).toHaveAttribute("aria-pressed", "true");
  });
});
