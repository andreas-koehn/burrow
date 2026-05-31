import { render, screen, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import {
  Button,
  Switch,
  DropdownMenu,
  EmptyState,
  ErrorNotice,
  SkeletonRows,
  NotAuthorized,
} from "./index";

describe("ds primitives", () => {
  it("Button renders variant class and fires onClick", () => {
    const fn = vi.fn();
    render(<Button variant="primary" onClick={fn}>Go</Button>);
    const b = screen.getByRole("button", { name: "Go" });
    expect(b.className).toContain("btn-primary");
    fireEvent.click(b);
    expect(fn).toHaveBeenCalled();
  });
  it("disabled primary button keeps its btn-primary class and the disabled attribute", () => {
    render(<Button variant="primary" disabled>Go</Button>);
    const b = screen.getByRole("button", { name: "Go" });
    expect(b).toBeDisabled();
    expect(b.className).toContain("btn-primary");
  });
  it("Switch toggles via keyboard/click and exposes role=switch", () => {
    const fn = vi.fn();
    render(<Switch checked={false} onChange={fn} aria-label="t" />);
    fireEvent.click(screen.getByRole("switch"));
    expect(fn).toHaveBeenCalledWith(true);
  });
  it("DropdownMenu opens and selects an item", () => {
    const fn = vi.fn();
    render(<DropdownMenu trigger={<button>⋯</button>} items={[{ label: "Del", onSelect: fn }]} />);
    fireEvent.click(screen.getByText("⋯"));
    fireEvent.click(screen.getByText("Del"));
    expect(fn).toHaveBeenCalled();
  });
});

describe("ds states", () => {
  it("EmptyState renders .state-card with its title and message", () => {
    const { container } = render(
      <EmptyState title="No live tunnels">Run burrow connect.</EmptyState>,
    );
    expect(container.querySelector(".state-card")).toBeTruthy();
    expect(screen.getByText("No live tunnels")).toBeTruthy();
    expect(screen.getByText("Run burrow connect.")).toBeTruthy();
  });
  it("ErrorNotice renders .notice-inline with role=alert by default", () => {
    const { container } = render(<ErrorNotice>Couldn't load.</ErrorNotice>);
    expect(container.querySelector(".notice-inline")).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.getByText("Couldn't load.")).toBeTruthy();
  });
  it("SkeletonRows n={3} renders 3 shimmer rows using .skel", () => {
    const { container } = render(<SkeletonRows n={3} />);
    expect(container.querySelectorAll(".row").length).toBe(3);
    expect(container.querySelectorAll(".skel").length).toBeGreaterThan(0);
  });
  it("NotAuthorized renders .state-card with the supplied copy", () => {
    const { container } = render(<NotAuthorized title="Admin access required." />);
    expect(container.querySelector(".state-card")).toBeTruthy();
    expect(screen.getByText("Admin access required.")).toBeTruthy();
  });
});
