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

  it("shows a server-side error in preference to the local one", () => {
    render(<SlugField id="s" value="taken" onChange={() => {}} error="slug already in use" />);
    expect(screen.getByRole("alert")).toHaveTextContent("slug already in use");
  });
});
