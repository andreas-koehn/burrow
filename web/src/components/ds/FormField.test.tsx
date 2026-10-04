import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { FormField } from "./FormField";
import { Input } from "./Input";

describe("FormField", () => {
  it("wraps a label + input and links them via htmlFor", () => {
    render(
      <FormField label="Email" htmlFor="email">
        <Input id="email" defaultValue="" />
      </FormField>,
    );
    const input = screen.getByLabelText("Email");
    expect(input.id).toBe("email");
  });

  it("renders help text in a .help element", () => {
    const { container } = render(
      <FormField label="Name" htmlFor="name" help="Lowercase letters only.">
        <Input id="name" />
      </FormField>,
    );
    const help = container.querySelector(".form-field .help");
    expect(help?.textContent).toBe("Lowercase letters only.");
  });

  it("renders error text in a .error element with role=alert", () => {
    const { container } = render(
      <FormField label="Port" htmlFor="port" error="Must be 1-65535.">
        <Input id="port" />
      </FormField>,
    );
    const err = container.querySelector(".form-field .error");
    expect(err?.textContent).toBe("Must be 1-65535.");
    expect(err?.getAttribute("role")).toBe("alert");
  });

  it("applies a field-w-* width class and never a bare Tailwind w-* utility", () => {
    const { container: c1 } = render(
      <FormField label="A" htmlFor="a" w="sm"><Input id="a" /></FormField>,
    );
    const sm = c1.querySelector(".form-field")!;
    expect(sm.classList.contains("field-w-sm")).toBe(true);

    const { container: c2 } = render(
      <FormField label="B" htmlFor="b" w="md"><Input id="b" /></FormField>,
    );
    const md = c2.querySelector(".form-field")!;
    expect(md.classList.contains("field-w-md")).toBe(true);

    const { container: c3 } = render(
      <FormField label="C" htmlFor="c"><Input id="c" /></FormField>,
    );
    expect(c3.querySelector(".form-field")!.classList.contains("field-w-full")).toBe(true);

    for (const el of [sm, md]) {
      for (const cls of Array.from(el.classList)) {
        expect(cls).not.toMatch(/^w-(sm|md|lg|full)$/);
      }
    }
  });
});
