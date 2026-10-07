import { useState } from "react";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
  Tabs,
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
    fireEvent.click(screen.getByRole("combobox"));
    const option = screen.getByRole("option", { name: "Beta" });
    fireEvent.mouseDown(option);
    fireEvent.click(option);
    expect(fn).toHaveBeenCalledWith("b");
    expect(screen.queryByRole("listbox")).toBeNull();
  });
  it("Select is operated with the keyboard alone", async () => {
    const fn = vi.fn();
    const user = userEvent.setup();
    const three = [...SCOPES, { value: "c", label: "Gamma" }];
    render(<Select options={three} value="a" onChange={fn} />);
    // The select-only combobox pattern: aria-activedescendant needs the role.
    const trigger = screen.getByRole("combobox");
    expect(trigger).toHaveAttribute("aria-haspopup", "listbox");
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    trigger.focus();
    // Arrow down opens the list on the chosen option; focus stays on the trigger.
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute("aria-expanded", "true");
    expect(trigger).toHaveAttribute("aria-controls", screen.getByRole("listbox").id);
    const active = () => document.getElementById(trigger.getAttribute("aria-activedescendant") ?? "");
    expect(active()).toHaveTextContent("Alpha");
    await user.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}");
    expect(active()).toHaveTextContent("Gamma"); // stops at the end
    await user.keyboard("{Home}");
    expect(active()).toHaveTextContent("Alpha");
    // A letter goes to the next option that starts with it, and round again.
    await user.keyboard("g");
    expect(active()).toHaveTextContent("Gamma");
    await user.keyboard("b");
    expect(active()).toHaveTextContent("Beta");
    await user.keyboard("x");
    expect(active()).toHaveTextContent("Beta");
    await user.keyboard("{End}{ArrowUp}");
    expect(active()).toHaveTextContent("Beta");
    expect(active()?.className).toContain("is-focus");
    await user.keyboard("{Enter}");
    expect(fn).toHaveBeenCalledTimes(1);
    expect(fn).toHaveBeenCalledWith("b");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(trigger).toHaveFocus();
    expect(trigger).not.toHaveAttribute("aria-activedescendant");
    // Enter opens it, Space chooses, Escape closes without choosing.
    await user.keyboard("{Enter}");
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    await user.keyboard("{ArrowDown}{ArrowDown} ");
    expect(fn).toHaveBeenLastCalledWith("c");
    await user.keyboard("{ArrowDown}");
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(fn).toHaveBeenCalledTimes(2);
    expect(trigger).toHaveFocus();
  });

  // The list lives on <body>: without this, focus would drop to <body> and
  // the next Tab would leave the dialog the Select sits in.
  it("Select keeps focus on its trigger when an option is chosen", () => {
    render(<Select options={SCOPES} value="a" />);
    const trigger = screen.getByRole("combobox");
    fireEvent.click(trigger);
    const list = screen.getByRole("listbox");
    expect(list.className).toContain("select-list");
    const option = screen.getByRole("option", { name: "Beta" });
    // mousedown is cancelled so the browser never moves focus off the trigger.
    expect(fireEvent.mouseDown(option)).toBe(false);
    fireEvent.click(option);
    expect(trigger).toHaveFocus();
  });
  it("Select closes on outside mousedown but not on mousedown inside the list", () => {
    render(<Select options={SCOPES} value="a" />);
    fireEvent.click(screen.getByRole("combobox"));
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
  it("Dialog keeps Tab and Shift+Tab inside, skipping disabled buttons", async () => {
    render(
      <>
        <button>Outside before</button>
        <Dialog
          open
          title="Edit"
          onOpenChange={() => {}}
          footer={<><Button>Cancel</Button><Button disabled>Save</Button></>}
        >
          <label htmlFor="trap-name">Name</label>
          <input id="trap-name" />
        </Dialog>
        <button>Outside after</button>
      </>,
    );
    // The dialog takes its initial focus on a short timer. Let it land first:
    // fired later, with focus parked outside, it would move the focus itself.
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
    const close = screen.getByRole("button", { name: "Close dialog" });
    const cancel = screen.getByRole("button", { name: "Cancel" });
    cancel.focus();
    // Cancel is the last control that can take focus: Save is disabled.
    await userEvent.tab();
    expect(close).toHaveFocus();
    await userEvent.tab({ shift: true });
    expect(cancel).toHaveFocus();
    // Focus that got out (a click on the page behind) is brought back in.
    screen.getByRole("button", { name: "Outside after" }).focus();
    await userEvent.tab();
    expect(close).toHaveFocus();
    screen.getByRole("button", { name: "Outside before" }).focus();
    await userEvent.tab({ shift: true });
    expect(cancel).toHaveFocus();
  });
  it("Dialog in a dialog: only the inner one holds the focus", async () => {
    render(
      <Dialog open title="Outer" onOpenChange={() => {}} footer={<Button>Outer cancel</Button>}>
        <Dialog open title="Inner" footer={<><Button>Back</Button><Button>Done</Button></>} />
      </Dialog>,
    );
    screen.getByRole("button", { name: "Done" }).focus();
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Back" })).toHaveFocus();
    await userEvent.tab({ shift: true });
    expect(screen.getByRole("button", { name: "Done" })).toHaveFocus();
  });
  it("Dialog does not pull focus out of a list its Select portals to the body", async () => {
    render(<DialogWithSelect onOpenChange={() => {}} />);
    fireEvent.click(screen.getByLabelText("Scope"));
    const option = screen.getByRole("option", { name: "Beta" });
    option.tabIndex = -1;
    option.focus();
    fireEvent.keyDown(option, { key: "Tab" });
    expect(option).toHaveFocus();
  });
  it("Tabs: arrow keys move the focus with the selection", async () => {
    function Harness() {
      const [v, setV] = useState("a");
      return <Tabs value={v} onChange={setV} tabs={[{ value: "a", label: "One" }, { value: "b", label: "Two" }]} />;
    }
    render(<Harness />);
    screen.getByRole("tab", { name: "One" }).focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "Two" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "Two" })).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(screen.getByRole("tab", { name: "One" })).toHaveFocus();
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
