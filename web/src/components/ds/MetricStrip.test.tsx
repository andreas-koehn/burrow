import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { MetricStrip, MetricTile } from "./MetricStrip";

describe("MetricStrip", () => {
  it("renders role=list with the given aria-label", () => {
    render(
      <MetricStrip ariaLabel="Spend by window">
        <MetricTile label="Today" value="$0.00" />
      </MetricStrip>,
    );
    const strip = screen.getByRole("list", { name: "Spend by window" });
    expect(strip.className).toContain("metric-strip");
  });

  it("MetricTile renders label, value, and optional sub", () => {
    const { container } = render(
      <MetricStrip ariaLabel="x">
        <MetricTile label="Week" value="$1.23" sub="42 req" />
      </MetricStrip>,
    );
    const tile = container.querySelector(".metric-tile");
    expect(tile).not.toBeNull();
    expect(tile?.querySelector(".label")?.textContent).toBe("Week");
    expect(tile?.querySelector(".value")?.textContent).toBe("$1.23");
    expect(tile?.querySelector(".sub")?.textContent).toBe("42 req");
    expect(tile?.getAttribute("role")).toBe("listitem");
  });

  it("MetricTile omits .sub when not provided", () => {
    const { container } = render(
      <MetricStrip ariaLabel="x">
        <MetricTile label="Today" value="$0.00" />
      </MetricStrip>,
    );
    expect(container.querySelector(".metric-tile .sub")).toBeNull();
  });

  it("MetricTile renders trailing children after .sub (for pct-bar etc.)", () => {
    const { container } = render(
      <MetricStrip ariaLabel="x">
        <MetricTile label="Today" value="$0.00">
          <div data-testid="bar" className="pct-bar" />
        </MetricTile>
      </MetricStrip>,
    );
    expect(container.querySelector('[data-testid="bar"]')).not.toBeNull();
  });

  it("MetricTile with `to` is a link around label, value and sub; the tile stays the list item", () => {
    render(
      <MemoryRouter>
        <MetricStrip ariaLabel="x">
          <MetricTile label="Services" value="4" sub="saved" to="/services" />
          <MetricTile label="Plain" value="1" />
        </MetricStrip>
      </MemoryRouter>,
    );
    const items = screen.getAllByRole("listitem");
    const link = screen.getByRole("link", { name: /Services/ });
    expect(link).toHaveAttribute("href", "/services");
    expect(items[0]).toContainElement(link);
    expect(link.querySelector(".value")?.textContent).toBe("4");
    expect(link.querySelector(".sub")?.textContent).toBe("saved");
    expect(items[1]!.querySelector("a")).toBeNull();
  });
});
