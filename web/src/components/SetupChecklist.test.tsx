import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { SetupChecklist, type ChecklistStep } from "./SetupChecklist";

const STEPS: ChecklistStep[] = [
  { id: "a", title: "First", description: "Do the first thing.", done: true, action: { label: "Open first", to: "/first" } },
  { id: "b", title: "Second", description: "Do the second thing.", done: false, action: { label: "Open second", to: "/second" } },
  { id: "c", title: "Third", description: "Do the third thing.", done: true },
  { id: "d", title: "Fourth", description: "Do the fourth thing.", done: false, content: <p>Fourth body</p> },
];

const mount = (steps: ChecklistStep[]) =>
  render(<MemoryRouter><SetupChecklist title="Set up things" steps={steps} /></MemoryRouter>);

describe("SetupChecklist", () => {
  it("is a list named by its title; each step shows title, description and its state", () => {
    mount(STEPS);
    expect(screen.getByRole("heading", { name: "Set up things" })).toBeInTheDocument();
    const items = within(screen.getByRole("list", { name: "Set up things" })).getAllByRole("listitem");
    expect(items).toHaveLength(4);
    expect(items.map((li) => within(li).getByRole("button").textContent)).toEqual(["First", "Second", "Third", "Fourth"]);
    STEPS.forEach((s, i) => expect(within(items[i]!).getByText(s.description)).toBeInTheDocument());
    expect(items.map((li) => li.querySelector(".visually-hidden")?.textContent)).toEqual(["done", "to do", "done", "to do"]);
  });

  it("opens the first open step only, showing its action link", () => {
    mount(STEPS);
    const expanded = (name: string) => screen.getByRole("button", { name }).getAttribute("aria-expanded");
    expect(["First", "Second", "Third", "Fourth"].map(expanded)).toEqual(["false", "true", "false", "false"]);
    expect(screen.getByRole("link", { name: "Open second" })).toHaveAttribute("href", "/second");
    expect(screen.queryByRole("link", { name: "Open first" })).toBeNull();
    expect(screen.queryByText("Fourth body")).toBeNull();
  });

  it("shows a step's content when it is the first open one", () => {
    mount(STEPS.map((s) => (s.id === "b" ? { ...s, done: true } : s)));
    expect(screen.getByRole("button", { name: "Fourth" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByText("Fourth body")).toBeInTheDocument();
  });

  it("a step opens and closes on click", async () => {
    mount(STEPS);
    await userEvent.click(screen.getByRole("button", { name: "Fourth" }));
    expect(screen.getByText("Fourth body")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Second" }));
    expect(screen.getByRole("button", { name: "Second" })).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("link", { name: "Open second" })).toBeNull();
  });

  it("says how far the setup is", () => {
    mount(STEPS);
    expect(screen.getByText("2 of 4 done")).toBeInTheDocument();
  });

  it("renders nothing when every step is done", () => {
    const { container } = mount(STEPS.map((s) => ({ ...s, done: true })));
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing without steps", () => {
    const { container } = mount([]);
    expect(container).toBeEmptyDOMElement();
  });
});
