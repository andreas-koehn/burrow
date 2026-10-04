import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { PageHeader } from "./PageHeader";

describe("PageHeader", () => {
  it("renders title as an h1 with no subtitle/actions", () => {
    const { container } = render(<PageHeader title="Tunnels" />);
    expect(screen.getByRole("heading", { level: 1, name: "Tunnels" })).toBeInTheDocument();
    expect(container.querySelector(".page-header")).not.toBeNull();
    expect(container.querySelector(".page-header > .actions")).toBeNull();
    expect(container.querySelector(".page-header .left p")).toBeNull();
  });

  it("renders subtitle as a <p> when supplied", () => {
    render(<PageHeader title="Cost & budgets" subtitle="Estimates from the pricing table." />);
    expect(screen.getByText(/Estimates from the pricing table\./)).toBeInTheDocument();
  });

  it("renders actions in a right-aligned slot", () => {
    const { container } = render(
      <PageHeader title="Webhooks" actions={<button>New webhook</button>} />,
    );
    const actions = container.querySelector(".page-header > .actions");
    expect(actions).not.toBeNull();
    expect(actions?.querySelector("button")?.textContent).toBe("New webhook");
  });
});
