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

  it("button aria-describedby matches the tooltip id when open", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    // Open the tooltip via focus
    fireEvent.focus(button);

    const tooltip = screen.getByRole("tooltip");
    expect(tooltip).toBeInTheDocument();
    expect(button.getAttribute("aria-describedby")).toBe(tooltip.id);
  });

  it("tooltip content is visible and associated after focus", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    fireEvent.focus(button);

    expect(screen.getByRole("tooltip")).toHaveTextContent("durable config");
  });

  it("tooltip is not in the DOM when closed", () => {
    render(<InfoHint label="Service" content="durable config" />);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("Escape key hides the tooltip", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    fireEvent.focus(button);
    expect(screen.getByRole("tooltip")).toBeInTheDocument();

    fireEvent.keyDown(button, { key: "Escape" });
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("mouseenter/mouseleave toggle the tooltip", () => {
    render(<InfoHint label="Service" content="durable config" />);
    const button = screen.getByRole("button", { name: /what is service\?/i });

    fireEvent.mouseEnter(button);
    expect(screen.getByRole("tooltip")).toBeInTheDocument();

    fireEvent.mouseLeave(button);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });
});
