import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { LogView, type LogColumn, type LogViewProps } from "./LogView";

interface Row { id: string; name: string; size: number }

const ROWS: Row[] = [
  { id: "a", name: "alpha", size: 10 },
  { id: "b", name: "beta", size: 20 },
];
const COLUMNS: LogColumn<Row>[] = [
  { id: "name", header: "Name", cell: (r) => r.name },
  { id: "size", header: "Size", cell: (r) => r.size, numeric: true },
];

function mount(over: Partial<LogViewProps<Row>> = {}) {
  const props: LogViewProps<Row> = {
    label: "Things",
    rows: ROWS,
    rowKey: (r) => r.id,
    columns: COLUMNS,
    isLoading: false,
    error: null,
    onRetry: vi.fn(),
    range: "24h",
    onRangeChange: vi.fn(),
    search: "",
    onSearchChange: vi.fn(),
    empty: <p>nothing here</p>,
    ...over,
  };
  return { props, ...render(<LogView {...props} />) };
}

describe("LogView", () => {
  it("renders a table named by its label, numeric columns right-aligned by class", () => {
    mount();
    const table = screen.getByRole("table", { name: "Things" });
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Name", "Size"]);
    expect(within(table).getAllByRole("row")).toHaveLength(3);
    const size = within(table).getByRole("columnheader", { name: "Size" });
    expect(size).toHaveClass("col-num");
    expect(size).not.toHaveAttribute("style");
    const cell = within(table).getByRole("cell", { name: "20" });
    expect(cell).toHaveClass("col-num");
    expect(cell).not.toHaveAttribute("style");
    expect(within(table).getByRole("cell", { name: "beta" })).not.toHaveClass("col-num");
  });

  it("marks every column header as a header of its column", () => {
    mount();
    const headers = within(screen.getByRole("table", { name: "Things" })).getAllByRole("columnheader");
    expect(headers).toHaveLength(COLUMNS.length);
    for (const h of headers) {
      expect(h.tagName).toBe("TH");
      expect(h).toHaveAttribute("scope", "col");
    }
  });

  it("shows skeleton rows and no table while loading", () => {
    const { container } = mount({ rows: undefined, isLoading: true });
    expect(container.querySelector(".skel")).not.toBeNull();
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.queryByText("nothing here")).toBeNull();
  });

  it("shows an error notice whose Retry button calls onRetry", async () => {
    const { props } = mount({ rows: undefined, error: new Error("boom") });
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("Couldn't load things");
    expect(screen.queryByRole("table")).toBeNull();
    await userEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(props.onRetry).toHaveBeenCalledTimes(1);
  });

  it("keeps the rows on screen when a refresh fails", () => {
    mount({ error: new Error("boom") });
    expect(screen.getByRole("table", { name: "Things" })).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows the empty node and no table for an empty list", () => {
    mount({ rows: [] });
    expect(screen.getByText("nothing here")).toBeInTheDocument();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("offers the time range as a radiogroup and reports the choice", async () => {
    const { props } = mount();
    const group = screen.getByRole("radiogroup", { name: "Time range" });
    expect(within(group).getAllByRole("radio").map((r) => r.textContent)).toEqual(["15 min", "1 hour", "24 hours", "7 days", "All"]);
    expect(within(group).getByRole("radio", { name: "24 hours" })).toBeChecked();
    await userEvent.click(within(group).getByRole("radio", { name: "7 days" }));
    expect(props.onRangeChange).toHaveBeenCalledWith("7d");
  });

  it("moves through the time range with the arrow keys", async () => {
    const { props } = mount();
    screen.getByRole("radio", { name: "24 hours" }).focus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(props.onRangeChange).toHaveBeenLastCalledWith("1h");
    await userEvent.keyboard("{ArrowRight}");
    expect(props.onRangeChange).toHaveBeenLastCalledWith("7d");
    await userEvent.click(screen.getByRole("radio", { name: "All" }));
    expect(props.onRangeChange).toHaveBeenLastCalledWith("all");
  });

  it("has a search input labelled Filter that reports what is typed", async () => {
    const { props } = mount();
    await userEvent.type(screen.getByRole("searchbox", { name: "Filter" }), "x");
    expect(props.onSearchChange).toHaveBeenCalledWith("x");
  });

  it("renders the extra filter controls", () => {
    mount({ filters: <button type="button">Pick a service</button> });
    expect(screen.getByRole("button", { name: "Pick a service" })).toBeInTheDocument();
  });

  const withDetail = { detail: (r: Row) => <p>detail of {r.name}</p> };
  const toggleOf = (name: RegExp) => within(screen.getByRole("row", { name })).getByRole("button", { name: "Show details" });

  it("a row with detail has a button that says whether the detail is open and what it controls", async () => {
    mount(withDetail);
    const button = toggleOf(/beta/);
    expect(button).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    const panel = screen.getByRole("region", { name: "Things details" });
    expect(button).toHaveAttribute("aria-controls", panel.id);
    expect(panel).toHaveTextContent("detail of beta");
    expect(panel).toHaveFocus();
    expect(toggleOf(/alpha/)).toHaveAttribute("aria-expanded", "false");
    // A plain table has no selectable rows; the row is neither a tab stop nor "selected".
    const row = screen.getByRole("row", { name: /beta/ });
    expect(row).not.toHaveAttribute("aria-selected");
    expect(row).not.toHaveAttribute("tabindex");
  });

  it("names each row's button after the row when the page says how", () => {
    mount({ ...withDetail, rowLabel: (r) => r.name });
    expect(screen.getByRole("button", { name: "Show details for alpha" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show details for beta" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Show details" })).toBeNull();
  });

  it("Escape closes the detail and returns the focus to the row's button", async () => {
    mount(withDetail);
    await userEvent.click(toggleOf(/beta/));
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByText(/detail of/)).toBeNull();
    expect(toggleOf(/beta/)).toHaveFocus();
    expect(toggleOf(/beta/)).toHaveAttribute("aria-expanded", "false");
  });

  it.each([["Enter", "{Enter}"], ["Space", " "]])("opens and closes the detail from the keyboard with %s", async (_name, key) => {
    mount(withDetail);
    toggleOf(/alpha/).focus();
    await userEvent.keyboard(key);
    expect(screen.getByText("detail of alpha")).toBeInTheDocument();
    toggleOf(/alpha/).focus();
    await userEvent.keyboard(key);
    expect(screen.queryByText(/detail of/)).toBeNull();
  });

  it("a click anywhere on the row toggles its detail", async () => {
    mount(withDetail);
    const cell = screen.getByRole("cell", { name: "20" });
    await userEvent.click(cell);
    expect(screen.getByText("detail of beta")).toBeInTheDocument();
    await userEvent.click(cell);
    expect(screen.queryByText(/detail of/)).toBeNull();
  });

  it("closes the detail with its Close button", async () => {
    mount(withDetail);
    await userEvent.click(toggleOf(/alpha/));
    await userEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByText(/detail of/)).toBeNull();
    expect(toggleOf(/alpha/)).toHaveFocus();
  });

  it("when the open row leaves the list the panel goes, the focus stays in the view and the row does not reopen", async () => {
    const { props, rerender } = mount(withDetail);
    await userEvent.click(toggleOf(/beta/));
    expect(screen.getByRole("region", { name: "Things details" })).toHaveFocus();
    rerender(<LogView {...props} rows={[ROWS[0]]} />);
    expect(screen.queryByText(/detail of/)).toBeNull();
    expect(screen.getByRole("group", { name: "Things log" })).toHaveFocus();
    rerender(<LogView {...props} rows={ROWS} />);
    expect(screen.queryByText(/detail of/)).toBeNull();
    expect(toggleOf(/beta/)).toHaveAttribute("aria-expanded", "false");
  });

  it("rows have no detail button when there is no detail", () => {
    mount();
    expect(screen.queryByRole("button", { name: "Show details" })).toBeNull();
    expect(screen.getByRole("row", { name: /alpha/ })).not.toHaveAttribute("tabindex");
  });

  it("offers Load more only when there is more", async () => {
    const first = mount();
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
    first.unmount();
    const { props } = mount({ hasMore: true, onLoadMore: vi.fn() });
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(props.onLoadMore).toHaveBeenCalledTimes(1);
  });
});
