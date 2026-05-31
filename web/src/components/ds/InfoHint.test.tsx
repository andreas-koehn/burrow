import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { InfoHint } from "./InfoHint";

describe("InfoHint", () => {
  it("renders a button with the correct aria-label", () => {
    render(<InfoHint label="Service" content="durable config" />);
    expect(
      screen.getByRole("button", { name: /what is service\?/i }),
    ).toBeInTheDocument();
  });

  it("button aria-describedby always points at the tooltip id (stable association)", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });
    // The tooltip span is always in the DOM (hidden=true to include hidden elements)
    const tooltip = screen.getByRole("tooltip", { hidden: true });
    expect(button.getAttribute("aria-describedby")).toBe(tooltip.id);
  });

  it("tooltip span is always present in the DOM (hidden when closed)", () => {
    render(<InfoHint label="Service" content="durable config" />);
    // Query with hidden:true so we find it even when the hidden attr is set
    const tooltip = screen.getByRole("tooltip", { hidden: true });
    expect(tooltip).toBeInTheDocument();
    expect(tooltip).toHaveAttribute("hidden");
  });

  it("tooltip content is visible and associated after focus", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    fireEvent.focus(button);

    // When open, tooltip is no longer hidden — getByRole without hidden:true works
    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toHaveTextContent("durable config");
    expect(tooltip).not.toHaveAttribute("hidden");
  });

  it("tooltip is hidden (not absent) when closed", () => {
    render(<InfoHint label="Service" content="durable config" />);
    // Always in DOM; use hidden:true to find it
    const tooltip = screen.getByRole("tooltip", { hidden: true });
    expect(tooltip).toHaveAttribute("hidden");
  });

  it("Escape key hides the tooltip (sets hidden attr)", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    fireEvent.focus(button);
    // After focus: visible, no hidden attr
    expect(screen.getByRole("tooltip")).not.toHaveAttribute("hidden");

    fireEvent.keyDown(button, { key: "Escape" });
    // After Escape: still in DOM, but hidden attr set
    expect(screen.getByRole("tooltip", { hidden: true })).toHaveAttribute("hidden");
  });

  it("mouseenter/mouseleave toggle the hidden attribute", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    fireEvent.mouseEnter(button);
    // Visible after mouseenter
    expect(screen.getByRole("tooltip")).not.toHaveAttribute("hidden");

    fireEvent.mouseLeave(button);
    // Hidden after mouseleave — use hidden:true to query
    expect(screen.getByRole("tooltip", { hidden: true })).toHaveAttribute("hidden");
  });
});
