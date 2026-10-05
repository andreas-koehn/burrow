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
    expect(within(group).getAllByRole("radio").map((r) => r.textContent)).toEqual(["15 min", "1 hour", "24 hours", "7 days"]);
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

  it("opens a row's detail on click; Escape closes it and returns the focus to the row", async () => {
    mount({ detail: (r) => <p>detail of {r.name}</p> });
    expect(screen.queryByText(/detail of/)).toBeNull();
    const row = screen.getByRole("row", { name: /beta/ });
    await userEvent.click(row);
    const panel = screen.getByRole("region", { name: "Things details" });
    expect(panel).toHaveTextContent("detail of beta");
    expect(panel).toHaveFocus();
    expect(row).toHaveAttribute("aria-selected", "true");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByText(/detail of/)).toBeNull();
    expect(row).toHaveFocus();
  });

  it.each([["Enter", "{Enter}"], ["Space", " "]])("opens the focused row's detail with %s", async (_name, key) => {
    mount({ detail: (r) => <p>detail of {r.name}</p> });
    const row = screen.getByRole("row", { name: /alpha/ });
    row.focus();
    await userEvent.keyboard(key);
    expect(screen.getByText("detail of alpha")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(row).toHaveFocus();
  });

  it("closes the detail with its Close button", async () => {
    mount({ detail: (r) => <p>detail of {r.name}</p> });
    const row = screen.getByRole("row", { name: /alpha/ });
    await userEvent.click(row);
    await userEvent.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByText(/detail of/)).toBeNull();
    expect(row).toHaveFocus();
  });

  it("rows are not focusable when there is no detail", () => {
    mount();
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
