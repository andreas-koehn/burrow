import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { StatusBadge } from "./StatusBadge";
import type { State } from "@/lib/status";

const CASES: [State, string, string][] = [
  ["online", "Online", "status-online"],
  ["offline", "Offline", "status-offline"],
  ["never_connected", "Never connected", "status-never"],
  ["ready", "Ready", "status-online"],
  ["not_configured", "Not configured", "status-never"],
];

describe("StatusBadge", () => {
  it.each(CASES)("%s is written out as %s", (state, label, kind) => {
    const { container } = render(<StatusBadge state={state} />);
    const badge = screen.getByText(label);
    expect(badge).toHaveClass("badge", kind);
    // The word is the whole content: nothing is left to colour or a tooltip.
    expect(container.textContent).toBe(label);
    expect(container.querySelector("[title]")).toBeNull();
    expect(container.querySelector("[aria-label]")).toBeNull();
  });

  it("hides the dot from a screen reader, which reads the word", () => {
    const { container } = render(<StatusBadge state="offline" />);
    const dot = container.querySelector(".dot");
    expect(dot).not.toBeNull();
    expect(dot).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByText("Offline")).not.toHaveAttribute("aria-hidden");
  });

  it("keeps a class it is given", () => {
    render(<StatusBadge state="online" className="extra" />);
    expect(screen.getByText("Online")).toHaveClass("badge", "status-online", "extra");
  });
});
