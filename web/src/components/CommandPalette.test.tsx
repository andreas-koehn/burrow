import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { ThemeProvider } from "@/components/theme-provider";
import { CommandPalette } from "@/components/CommandPalette";
import { setCsrfCookie } from "@/mocks/test-utils";

// Helper to render CommandPalette inside a router and query provider.
// Renders a "spy" route so we can detect navigation by checking current location.
function SpyHeading() {
  const loc = useLocation();
  return <div data-testid="spy-location">{loc.pathname}</div>;
}

interface RenderOptions {
  open?: boolean;
  isAdmin?: boolean;
  hasAiEndpoints?: boolean;
  firstHttpServiceId?: string;
  onOpenChange?: (open: boolean) => void;
}

function renderPalette({
  open = true,
  isAdmin = true,
  hasAiEndpoints = true,
  firstHttpServiceId = "svc_web01",
  onOpenChange = vi.fn(),
}: RenderOptions = {}) {
  setCsrfCookie();
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <ThemeProvider>
      <QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={["/"]}>
          <Routes>
            <Route
              path="/*"
              element={
                <>
                  <SpyHeading />
                  <CommandPalette
                    open={open}
                    onOpenChange={onOpenChange}
                    isAdmin={isAdmin}
                    hasAiEndpoints={hasAiEndpoints}
                    firstHttpServiceId={firstHttpServiceId}
                  />
                </>
              }
            />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    </ThemeProvider>,
  );
  return { ...result, onOpenChange, qc };
}

describe("CommandPalette — P6A.1: destinations", () => {
  it("Cl-3: open=true renders a role=dialog element", () => {
    renderPalette();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("does not render when open=false", () => {
    renderPalette({ open: false });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("renders a search input", () => {
    renderPalette();
    expect(screen.getByRole("searchbox")).toBeInTheDocument();
  });

  it("typing 'sett' filters to only the Settings row (admin)", async () => {
    renderPalette({ isAdmin: true });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "sett");
    // Settings destination should be visible (option name includes the group "Administration")
    expect(screen.getByRole("option", { name: /settings/i })).toBeInTheDocument();
    // Home should not be visible (no match on "sett")
    expect(screen.queryByRole("option", { name: /^home/i })).not.toBeInTheDocument();
  });

  it("typing 'zzz' shows 'No matches'", async () => {
    renderPalette();
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "zzz");
    expect(screen.getByText(/no matches/i)).toBeInTheDocument();
  });

  it("ArrowDown then Enter navigates to the focused item and calls onOpenChange(false)", async () => {
    const onOpenChange = vi.fn();
    renderPalette({ onOpenChange });

    // The onKeyDown handler is on the flex div wrapping the input and listbox.
    // The dialog-body > div is where we need to fire the event.
    const dialog = screen.getByRole("dialog");
    const keyTarget = dialog.querySelector(".dialog-body > div") as HTMLElement;
    expect(keyTarget).not.toBeNull();

    // Arrow down from first item (index 0 = Home) to second item (index 1).
    fireEvent.keyDown(keyTarget, { key: "ArrowDown" });
    // Press Enter to activate index 1.
    fireEvent.keyDown(keyTarget, { key: "Enter" });

    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("Enter on first item navigates and calls onOpenChange(false)", async () => {
    const onOpenChange = vi.fn();
    renderPalette({ onOpenChange });

    const dialog = screen.getByRole("dialog");
    const keyTarget = dialog.querySelector(".dialog-body > div") as HTMLElement;
    expect(keyTarget).not.toBeNull();

    // Enter on focusIdx=0 (first item = Home → "/")
    fireEvent.keyDown(keyTarget, { key: "Enter" });

    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("clicking an item calls onOpenChange(false)", async () => {
    const onOpenChange = vi.fn();
    renderPalette({ onOpenChange });
    const firstItem = screen.getAllByRole("option")[0];
    fireEvent.click(firstItem);
    await waitFor(() => {
      expect(onOpenChange).toHaveBeenCalledWith(false);
    });
  });

  it("excludes admin-only destinations when isAdmin=false", async () => {
    renderPalette({ isAdmin: false });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "users");
    // Users is admin-only — should not appear for non-admin
    await waitFor(() => {
      expect(screen.queryByRole("option", { name: /users/i })).not.toBeInTheDocument();
    });
  });

  it("includes admin-only destinations when isAdmin=true", async () => {
    renderPalette({ isAdmin: true });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "users");
    // The option accessible name includes the group label "Access control" too.
    expect(await screen.findByRole("option", { name: /users/i })).toBeInTheDocument();
  });
});

describe("CommandPalette — P6A.2: live entities (MSW seed)", () => {
  it("typing 'ollam' shows 'ollama — service' from MSW seed", async () => {
    renderPalette({ isAdmin: true });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "ollam");
    // MSW seed has service named 'ollama' (svc_ai001)
    expect(await screen.findByRole("option", { name: /ollama.*service/i })).toBeInTheDocument();
  });

  it("typing 'office' shows 'office-box-1 — client' from MSW seed (admin)", async () => {
    renderPalette({ isAdmin: true });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "office");
    // MSW seed has client with token_name 'office-box-1'
    expect(await screen.findByRole("option", { name: /office-box-1.*client/i })).toBeInTheDocument();
  });

  it("clients do not appear for non-admin (query disabled)", async () => {
    renderPalette({ isAdmin: false });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "office");
    // Wait briefly; clients should never appear for non-admin
    await act(async () => { await new Promise((r) => setTimeout(r, 50)); });
    expect(screen.queryByRole("option", { name: /office-box-1.*client/i })).not.toBeInTheDocument();
  });

  it("grafana service appears when typing 'graf'", async () => {
    renderPalette({ isAdmin: true });
    const input = screen.getByRole("searchbox");
    await userEvent.type(input, "graf");
    expect(await screen.findByRole("option", { name: /grafana.*service/i })).toBeInTheDocument();
  });
});
