import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { MemoryRouter } from "react-router-dom";
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

  it("renders a back link above the title when `back` is given (U1)", () => {
    render(
      <MemoryRouter>
        <PageHeader title="Retention" back={{ to: "/settings", label: "Settings" }} />
      </MemoryRouter>,
    );
    const link = screen.getByRole("link", { name: "Back to Settings" });
    expect(link).toHaveAttribute("href", "/settings");
    expect(link.className).toContain("page-back");
  });

  it("renders no back link by default", () => {
    render(<MemoryRouter><PageHeader title="Home" /></MemoryRouter>);
    expect(screen.queryByRole("link")).toBeNull();
  });
});
