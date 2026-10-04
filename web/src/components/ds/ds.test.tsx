import { useState } from "react";
import { render, screen, fireEvent } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import {
  Button,
  Dialog,
  Select,
  Switch,
  DropdownMenu,
  EmptyState,
  ErrorNotice,
  SkeletonRows,
  NotAuthorized,
  TableEmptyRow,
} from "./index";

const SCOPES = [
  { value: "a", label: "Alpha" },
  { value: "b", label: "Beta" },
];

function DialogWithSelect({ onOpenChange }: { onOpenChange: (open: boolean) => void }) {
  const [value, setValue] = useState("a");
  return (
    <Dialog open title="New rule" onOpenChange={onOpenChange}>
      <label htmlFor="scope">Scope</label>
      <Select id="scope" options={SCOPES} value={value} onChange={setValue} />
    </Dialog>
  );
}

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
  it("DropdownMenu renders its menu outside clipping ancestors (portal on body)", () => {
    const { container } = render(
      <div style={{ overflow: "hidden" }}>
        <DropdownMenu trigger={<button>⋯</button>} items={[{ label: "Inspect" }]} />
      </div>,
    );
    fireEvent.click(screen.getByText("⋯"));
    const menu = screen.getByRole("menu");
    expect(container.contains(menu)).toBe(false);
    expect(menu.parentElement).toBe(document.body);
    expect(menu.style.position).toBe("fixed");
  });
  it("DropdownMenu closes on outside mousedown but not on mousedown inside the menu", () => {
    render(<DropdownMenu trigger={<button>⋯</button>} items={[{ label: "Inspect" }]} />);
    fireEvent.click(screen.getByText("⋯"));
    fireEvent.mouseDown(screen.getByRole("menu"));
    expect(screen.queryByRole("menu")).not.toBeNull();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("menu")).toBeNull();
  });
  // A Dialog opened from a menu item captures document.activeElement while it
  // opens, so the trigger must already hold focus when onSelect runs.
  it("DropdownMenu returns focus to its trigger before a clicked item's onSelect runs", () => {
    let focusedDuringSelect: Element | null = null;
    const onSelect = vi.fn(() => { focusedDuringSelect = document.activeElement; });
    render(<DropdownMenu trigger={<button>⋯</button>} items={[{ label: "Edit", onSelect }]} />);
    const trigger = screen.getByText("⋯");
    fireEvent.click(trigger);
    expect(document.activeElement).not.toBe(trigger);
    fireEvent.click(screen.getByRole("menuitem", { name: "Edit" }));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(focusedDuringSelect).toBe(trigger);
    expect(document.activeElement).toBe(trigger);
    expect(screen.queryByRole("menu")).toBeNull();
  });
  it("DropdownMenu returns focus to its trigger when an item is selected with Enter", () => {
    let focusedDuringSelect: Element | null = null;
    const onSelect = vi.fn(() => { focusedDuringSelect = document.activeElement; });
    render(<DropdownMenu trigger={<button>⋯</button>} items={[{ label: "Edit", onSelect }]} />);
    const trigger = screen.getByText("⋯");
    fireEvent.click(trigger);
    expect(document.activeElement).not.toBe(trigger);
    fireEvent.keyDown(document, { key: "Enter" });
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(focusedDuringSelect).toBe(trigger);
    expect(document.activeElement).toBe(trigger);
  });
  it("Select renders its list outside clipping ancestors (portal on body)", () => {
    const { container } = render(
      <div style={{ overflow: "hidden" }}>
        <label htmlFor="scope">Scope</label>
        <Select id="scope" options={SCOPES} value="a" />
      </div>,
    );
    const trigger = screen.getByLabelText("Scope");
    expect(trigger).toHaveAttribute("aria-haspopup", "listbox");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(trigger);
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    const list = screen.getByRole("listbox");
    expect(container.contains(list)).toBe(false);
    expect(list.parentElement).toBe(document.body);
    expect(list.style.position).toBe("fixed");
    // Above dialogs (z-index 30), like the DropdownMenu menu.
    expect(Number(list.style.zIndex)).toBeGreaterThan(30);
    expect(screen.getByRole("option", { name: "Alpha" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("option", { name: "Beta" })).toHaveAttribute("aria-selected", "false");
  });
  it("Select selects an option on click and closes", () => {
    const fn = vi.fn();
    render(<Select options={SCOPES} value="a" onChange={fn} />);
    fireEvent.click(screen.getByRole("button"));
    const option = screen.getByRole("option", { name: "Beta" });
    fireEvent.mouseDown(option);
    fireEvent.click(option);
    expect(fn).toHaveBeenCalledWith("b");
    expect(screen.queryByRole("listbox")).toBeNull();
  });
  it("Select closes on outside mousedown but not on mousedown inside the list", () => {
    render(<Select options={SCOPES} value="a" />);
    fireEvent.click(screen.getByRole("button"));
    fireEvent.mouseDown(screen.getByRole("listbox"));
    expect(screen.queryByRole("listbox")).not.toBeNull();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("listbox")).toBeNull();
  });
  it("Select inside a Dialog: an option click selects without closing the dialog", () => {
    const onOpenChange = vi.fn();
    render(<DialogWithSelect onOpenChange={onOpenChange} />);
    const trigger = screen.getByLabelText("Scope");
    fireEvent.click(trigger);
    const option = screen.getByRole("option", { name: "Beta" });
    expect(screen.getByRole("dialog").contains(option)).toBe(false);
    fireEvent.mouseDown(option);
    fireEvent.click(option);
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(trigger).toHaveTextContent("Beta");
  });
  it("Select inside a Dialog: Escape closes only the list and keeps focus on the trigger", () => {
    const onOpenChange = vi.fn();
    render(<DialogWithSelect onOpenChange={onOpenChange} />);
    const trigger = screen.getByLabelText("Scope");
    trigger.focus();
    fireEvent.click(trigger);
    fireEvent.keyDown(trigger, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(onOpenChange).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(trigger).toHaveFocus();
    // With the list closed, Escape reaches the dialog again.
    fireEvent.keyDown(trigger, { key: "Escape" });
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
  it("TableEmptyRow spans the table and shows title + hint", () => {
    render(
      <table><tbody>
        <TableEmptyRow colSpan={4} title="No tokens yet.">Create one to connect a client.</TableEmptyRow>
      </tbody></table>,
    );
    const cell = screen.getByRole("cell");
    expect(cell).toHaveAttribute("colspan", "4");
    expect(cell.className).toContain("table-empty");
    expect(screen.getByText("No tokens yet.")).toBeInTheDocument();
    expect(screen.getByText("Create one to connect a client.")).toBeInTheDocument();
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
  it("ErrorNotice exposes its variant and uses a non-warning icon for info/success", () => {
    const { container, rerender } = render(<ErrorNotice variant="info">hello</ErrorNotice>);
    const root = container.querySelector(".notice-inline")!;
    expect(root.getAttribute("data-variant")).toBe("info");
    expect(root.querySelector("svg.lucide-info")).not.toBeNull();
    rerender(<ErrorNotice variant="success">ok</ErrorNotice>);
    expect(container.querySelector("svg.lucide-circle-check")).not.toBeNull();
    rerender(<ErrorNotice variant="warn">careful</ErrorNotice>);
    expect(container.querySelector("svg.lucide-triangle-alert")).not.toBeNull();
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
