import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { SubmitHint } from "./SubmitHint";

describe("SubmitHint", () => {
  it("shows the first thing that is missing", () => {
    render(<SubmitHint id="h" missing={[false, null, undefined, "", "Enter a name.", "Choose a provider."]} />);
    const hint = screen.getByRole("status");
    expect(hint).toHaveTextContent("Enter a name.");
    expect(hint).not.toHaveTextContent("Choose a provider.");
  });

  it("carries the id the button points at", () => {
    render(
      <>
        <SubmitHint id="nm-hint" missing={["Enter a name."]} />
        <button aria-describedby="nm-hint">Create</button>
      </>,
    );
    expect(screen.getByRole("status")).toHaveAttribute("id", "nm-hint");
    expect(screen.getByRole("button", { name: "Create" })).toHaveAccessibleDescription("Enter a name.");
  });

  it("renders nothing when nothing is missing", () => {
    const { container } = render(<SubmitHint id="h" missing={[false, null, undefined, ""]} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("follows what is missing as it changes", () => {
    const { rerender } = render(<SubmitHint id="h" missing={["Enter a name.", "Choose a model."]} />);
    rerender(<SubmitHint id="h" missing={[false, "Choose a model."]} />);
    expect(screen.getByRole("status")).toHaveTextContent("Choose a model.");
    rerender(<SubmitHint id="h" missing={[false, false]} />);
    expect(screen.queryByRole("status")).toBeNull();
  });
});
