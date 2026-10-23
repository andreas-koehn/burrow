import { describe, it, expect } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { AccessPolicyEditor } from "@/components/AccessPolicyEditor";

const SVC = "svc_graf01"; // seeded burrow_login service, policy ["user"]

describe("AccessPolicyEditor", () => {
  it("renders a chip per role with the current policy preselected", async () => {
    renderApp(<AccessPolicyEditor serviceId={SVC} />);
    const userChip = await screen.findByRole("button", { name: /^user$/i });
    const adminChip = screen.getByRole("button", { name: /^admin$/i });
    expect(userChip).toHaveAttribute("aria-pressed", "true");
    expect(adminChip).toHaveAttribute("aria-pressed", "false");
  });

  it("shows the verbatim SSO explainer", async () => {
    renderApp(<AccessPolicyEditor serviceId={SVC} />);
    expect(
      await screen.findByText(
        "Signed-in users whose role is listed may reach this service — SSO: one Burrow login covers every protected service their role allows.",
      ),
    ).toBeInTheDocument();
  });

  it("warns when the policy is emptied", async () => {
    renderApp(<AccessPolicyEditor serviceId={SVC} />);
    await userEvent.click(await screen.findByRole("button", { name: /^user$/i }));
    expect(
      await screen.findByText(
        "Empty policy — nobody can reach this service until a role is added.",
      ),
    ).toBeInTheDocument();
  });

  it("saves the selected roles", async () => {
    renderApp(<AccessPolicyEditor serviceId={SVC} />);
    await userEvent.click(await screen.findByRole("button", { name: /^admin$/i }));
    await userEvent.click(screen.getByRole("button", { name: /save/i }));
    expect(await screen.findByText(/access policy saved/i)).toBeInTheDocument();
  });

  it("surfaces an unknown-role 400 as a role=alert", async () => {
    server.use(
      http.put(`/api/v1/services/${SVC}/access-policy`, () =>
        HttpResponse.json({ error: 'unknown role "ghost"' }, { status: 400 }),
      ),
    );
    renderApp(<AccessPolicyEditor serviceId={SVC} />);
    await userEvent.click(await screen.findByRole("button", { name: /^admin$/i }));
    await userEvent.click(screen.getByRole("button", { name: /save/i }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/unknown role "ghost"/i),
    );
  });
});

describe("AccessPolicyEditor for forwarding", () => {
  it("explains an empty policy as owner only and names who may forward", async () => {
    renderApp(<AccessPolicyEditor serviceId="svc_pg001" forward />);
    expect(await screen.findByRole("heading", { name: "Who may forward" })).toBeInTheDocument();
    expect(screen.getByText("Empty policy: only the owner can forward to this service.")).toBeInTheDocument();
    expect(screen.getByText(/Signed-in users whose role is listed may forward to this service/)).toBeInTheDocument();
    expect(screen.queryByText(/nobody can reach/i)).toBeNull();
  });
});
