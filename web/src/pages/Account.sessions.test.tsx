import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import Account from "@/pages/Account";
import { parseUserAgent } from "@/lib/userAgent";

describe("Account active sessions", () => {
  beforeEach(() => vi.restoreAllMocks());

  it("lists sessions and marks the current one", async () => {
    renderApp(<Account />);
    const table = await screen.findByRole("table", { name: /active sessions/i });
    expect(within(table).getByText(/this device/i)).toBeInTheDocument();
    expect(within(table).getByText("198.51.100.4")).toBeInTheDocument();
  });

  it("actions column has a header and current session shows label (D-16)", async () => {
    renderApp(<Account />);
    const table = await screen.findByRole("table", { name: /active sessions/i });
    expect(within(table).getByRole("columnheader", { name: /actions/i })).toBeInTheDocument();
    const currentRow = within(table).getByText(/this device/i).closest("tr")!;
    expect(within(currentRow).getByText(/current session/i)).toBeInTheDocument();
  });

  it("revokes a non-current session", async () => {
    renderApp(<Account />);
    const table = await screen.findByRole("table", { name: /active sessions/i });
    const oldRow = within(table).getByText("198.51.100.4").closest("tr")!;
    await userEvent.click(within(oldRow).getByRole("button", { name: /revoke/i }));
    await waitFor(() => expect(screen.queryByText("198.51.100.4")).not.toBeInTheDocument());
  });

  it("signs out everywhere", async () => {
    renderApp(<Account />);
    await screen.findByRole("table", { name: /active sessions/i });
    await userEvent.click(screen.getByRole("button", { name: /sign out everywhere/i }));
    await waitFor(() => expect(screen.queryByText("198.51.100.4")).not.toBeInTheDocument());
  });

  it("sessions table renders browser · os, not raw UA (C5)", async () => {
    renderApp(<Account />);
    await waitFor(() => {
      expect(screen.getByText(/Chrome 148 · Windows/)).toBeInTheDocument();
      expect(screen.queryByText(/AppleWebKit/)).toBeNull();
    });
  });

  it("parseUserAgent unit: Chrome on Windows", () => {
    const out = parseUserAgent("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36");
    expect(out).toEqual({ browser: "Chrome 148", os: "Windows" });
  });
});
