import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect } from "vitest";
import { Combobox } from "./Combobox";
import type { ComboboxOption } from "./Combobox";
import { FormField } from "./FormField";

function Field({ options, emptyText, describedBy }: { options: ComboboxOption[]; emptyText?: string; describedBy?: string }) {
  const [value, setValue] = useState("");
  return (
    <FormField label="Model id" htmlFor="m">
      <Combobox id="m" value={value} onChange={setValue} options={options} emptyText={emptyText} aria-describedby={describedBy} />
    </FormField>
  );
}

const OPTIONS: ComboboxOption[] = [
  { value: "glm-5.1", label: "GLM 5.1", hint: "200k" },
  { value: "mistral" },
];

/** The suggestions the browser offers for the input: the options of its datalist. */
function offered(input: HTMLElement): HTMLOptionElement[] {
  const list = document.getElementById(input.getAttribute("list") ?? "");
  return Array.from(list?.querySelectorAll("option") ?? []);
}

describe("Combobox", () => {
  it("offers the options and accepts one", async () => {
    render(<Field options={OPTIONS} />);
    const input = screen.getByRole("combobox", { name: "Model id" });
    expect(offered(input).map((o) => o.value)).toEqual(["glm-5.1", "mistral"]);
    // A label and a hint are shown next to the value; the value alone is taken.
    expect(offered(input).map((o) => o.getAttribute("label"))).toEqual(["GLM 5.1 · 200k", null]);
    // Picking a suggestion sets the input's value in one step.
    fireEvent.change(input, { target: { value: "glm-5.1" } });
    expect(input).toHaveValue("glm-5.1");
  });

  it("accepts a value that is not an option", async () => {
    render(<Field options={OPTIONS} />);
    const input = screen.getByRole("combobox", { name: "Model id" });
    await userEvent.type(input, "qwen2.5:0.5b");
    expect(input).toHaveValue("qwen2.5:0.5b");
  });

  it("is labelled by the FormField around it", async () => {
    render(<Field options={OPTIONS} />);
    expect(screen.getByLabelText("Model id")).toHaveAttribute("id", "m");
  });

  it("shows the empty text when there are no options", async () => {
    const { rerender } = render(<Field options={[]} emptyText="Nothing to choose from yet." describedBy="m-help" />);
    const input = screen.getByLabelText("Model id");
    expect(screen.getByText("Nothing to choose from yet.")).toBeInTheDocument();
    // Read with the field, after what the caller describes it by.
    expect(input).toHaveAttribute("aria-describedby", "m-help m-empty");
    expect(offered(input)).toEqual([]);
    rerender(<Field options={OPTIONS} emptyText="Nothing to choose from yet." describedBy="m-help" />);
    expect(screen.queryByText("Nothing to choose from yet.")).toBeNull();
    expect(input).toHaveAttribute("aria-describedby", "m-help");
  });

  it("marks an invalid value and takes the monospace face", () => {
    render(<Combobox id="x" value="a b" onChange={() => {}} options={[]} mono invalid />);
    const input = screen.getByRole("combobox");
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input).toHaveClass("input", "mono", "is-invalid");
  });
});
