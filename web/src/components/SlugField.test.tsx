import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SlugField, slugError } from "./SlugField";

describe("slugError", () => {
  it("accepts valid slugs", () => {
    for (const s of ["abc", "p7baeh", "my-app-2"]) expect(slugError(s)).toBeNull();
  });
  it("rejects invalid slugs with the rule text", () => {
    for (const s of ["ab", "-abc", "abc-", "Abc", "a_b", "a b", "a".repeat(41)]) {
      expect(slugError(s)).toMatch(/3.40 characters/);
    }
  });
  it("treats empty as 'not chosen', not as an error", () => {
    expect(slugError("")).toBeNull();
  });
});

describe("SlugField", () => {
  it("previews the resulting URL and flags invalid input", async () => {
    const onChange = vi.fn();
    const { rerender } = render(<SlugField id="s" value="my-app" onChange={onChange} />);
    expect(screen.getByLabelText("URL slug")).toHaveValue("my-app");
    expect(screen.getByText(`${window.location.origin}/svc/my-app/`)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText("URL slug"), "X");
    expect(onChange).toHaveBeenLastCalledWith("my-appx"); // input lower-cases as you type

    rerender(<SlugField id="s" value="-bad" onChange={onChange} />);
    expect(screen.getByRole("alert")).toHaveTextContent(/3.40 characters/);
    expect(screen.getByLabelText("URL slug")).toHaveAttribute("aria-invalid", "true");
  });

  it("errorAfterBlur: the rule is named once the field was left, a server error at once", async () => {
    const onBlur = vi.fn();
    const { rerender } = render(<SlugField id="s" value="-bad" onChange={() => {}} onBlur={onBlur} errorAfterBlur />);
    const field = screen.getByLabelText("URL slug");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(field).not.toHaveAttribute("aria-invalid");

    await userEvent.click(field);
    await userEvent.tab();
    expect(onBlur).toHaveBeenCalledTimes(1);
    expect(screen.getByRole("alert")).toHaveTextContent(/3.40 characters/);
    expect(field).toHaveAttribute("aria-invalid", "true");

    rerender(<SlugField id="t" value="taken" onChange={() => {}} error="slug already in use" errorAfterBlur />);
    expect(screen.getByRole("alert")).toHaveTextContent("slug already in use");
  });

  it("takes a stricter rule from its owner", async () => {
    render(<SlugField id="s" value="v1x" onChange={() => {}} rule={(v) => (v === "v1x" ? "Reserved." : null)} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Reserved.");
    // A value the rule refuses has no address to preview.
    expect(screen.queryByText(/\/svc\/v1x\//)).toBeNull();
  });

  it("shows help while the field is empty, in place of the preview", () => {
    const { rerender } = render(<SlugField id="s" value="" onChange={() => {}} help="Leave empty to derive it." />);
    expect(screen.getByLabelText("URL slug")).toHaveAccessibleDescription("Leave empty to derive it.");
    rerender(<SlugField id="s" value="my-app" onChange={() => {}} help="Leave empty to derive it." />);
    expect(screen.getByLabelText("URL slug")).toHaveAccessibleDescription(`${window.location.origin}/svc/my-app/`);
  });

  it("shows a server-side error in preference to the local one", () => {
    render(<SlugField id="s" value="taken" onChange={() => {}} error="slug already in use" />);
    expect(screen.getByRole("alert")).toHaveTextContent("slug already in use");
  });
});
