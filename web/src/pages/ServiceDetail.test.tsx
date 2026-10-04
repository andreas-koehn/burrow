import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { Route, Routes } from "react-router-dom";
import ServiceDetail from "@/pages/ServiceDetail";

function mount() {
  return renderApp(
    <Routes>
      <Route path="/services/:id" element={<ServiceDetail />} />
    </Routes>,
    "/services/svc_ai001",
  );
}

describe("ServiceDetail page", () => {
  it("renders an idle service as a status-idle badge (C4)", async () => {
    renderApp(
      <Routes>
        <Route path="/services/:id" element={<ServiceDetail />} />
      </Routes>,
      "/services/svc_graf01",
    );
    const idle = await screen.findByText("idle");
    expect(idle).toHaveClass("badge", "status-idle");
    expect(idle).not.toHaveClass("muted");
  });

  it("shows the path URL in the meta strip with a copy button, no subdomain host", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    expect(screen.getByText("/svc/ai4m2q/")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Copy URL https://tunnels.example.com/svc/ai4m2q/" }),
    ).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/ai4m2q\.tunnels\.example\.com/);
  });

  it("renders service name in the heading", async () => {
    mount();
    const heading = await screen.findByRole("heading", { name: /ollama/i });
    expect(heading.textContent).toMatch(/ollama/i);
  });

  it("shows four tabs: Access, API keys, Upstream key, Custom domains", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    const tablist = screen.getByRole("tablist");
    const tabs = within(tablist).getAllByRole("tab");
    const labels = tabs.map((t) => t.textContent?.trim());
    expect(labels).toContain("Access");
    expect(labels).toContain("API keys");
    expect(labels).toContain("Upstream key");
    expect(labels).toContain("Custom domains");
  });

  it("Upstream key tab renders the binding fields when a binding exists", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    // Switch to Upstream key tab
    const upstreamTab = screen.getByRole("tab", { name: /upstream key/i });
    await userEvent.click(upstreamTab);
    // The binding for svc_ai001 has header_name=Authorization and header_format=Bearer {key}
    const headerNameInput = await screen.findByLabelText(/header name/i);
    expect((headerNameInput as HTMLInputElement).value).toBe("Authorization");
    const headerFormatInput = screen.getByLabelText(/header format/i);
    expect((headerFormatInput as HTMLInputElement).value).toBe("Bearer {key}");
  });

  // P5.4 — #upstream-key hash deep-link opens Upstream-key tab directly
  it("P5.4: #upstream-key hash makes Upstream-key tab active on load", async () => {
    renderApp(
      <Routes>
        <Route path="/services/:id" element={<ServiceDetail />} />
      </Routes>,
      "/services/svc_ai001#upstream-key",
    );
    await screen.findByRole("heading", { name: /ollama/i });
    // The Upstream key tab should be selected (aria-selected="true")
    const upstreamTab = screen.getByRole("tab", { name: /upstream key/i });
    expect(upstreamTab).toHaveAttribute("aria-selected", "true");
    // And its panel content should be visible (binding fields rendered immediately)
    const headerNameInput = await screen.findByLabelText(/header name/i);
    expect(headerNameInput).toBeInTheDocument();
  });

  // P5.4 — explainer link to AI endpoints in the Upstream-key tab
  it("P5.4: Upstream-key tab shows AI endpoints explainer link", async () => {
    mount();
    await screen.findByRole("heading", { name: /ollama/i });
    const upstreamTab = screen.getByRole("tab", { name: /upstream key/i });
    await userEvent.click(upstreamTab);
    // Explainer text + link to /ai/endpoints
    const link = await screen.findByRole("link", { name: /ai endpoints/i });
    expect(link).toHaveAttribute("href", "/ai/endpoints");
  });

  it("renders the API keys table only in the API keys tab (C7)", async () => {
    mount();
    // svc_ai001 is in api_key mode, the mode that used to embed the key list.
    await screen.findByRole("radiogroup", { name: "Access mode" });
    expect(screen.getByLabelText("API key header")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "New key" })).toBeNull();
    await userEvent.click(screen.getByRole("tab", { name: "API keys" }));
    expect(await screen.findByRole("button", { name: "New key" })).toBeInTheDocument();
  });
});
