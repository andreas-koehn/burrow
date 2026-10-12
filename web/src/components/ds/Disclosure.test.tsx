import { useState } from "react";
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Disclosure } from "./Disclosure";

function Harness({ start = false }: { start?: boolean }) {
  const [open, setOpen] = useState(start);
  return (
    <Disclosure label="Advanced" open={open} onOpenChange={setOpen}>
      <input aria-label="Timeout" />
    </Disclosure>
  );
}

describe("Disclosure", () => {
  it("keeps its content out of the page until it is opened", async () => {
    render(<Harness />);
    const toggle = screen.getByRole("button", { name: "Advanced" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByLabelText("Timeout")).toBeNull();

    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByLabelText("Timeout")).toBeInTheDocument();
    // The button names the region it opens.
    expect(document.getElementById(toggle.getAttribute("aria-controls")!)).toContainElement(screen.getByLabelText("Timeout"));

    await userEvent.click(toggle);
    expect(screen.queryByLabelText("Timeout")).toBeNull();
  });

  it("is opened from outside, for a field in it that needs attention", () => {
    render(<Harness start />);
    expect(screen.getByRole("button", { name: "Advanced" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByLabelText("Timeout")).toBeInTheDocument();
  });

  it("is a button that does not submit a form around it", () => {
    render(<Harness />);
    expect(screen.getByRole("button", { name: "Advanced" })).toHaveAttribute("type", "button");
  });
});
