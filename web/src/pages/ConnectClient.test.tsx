import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Routes, Route } from "react-router-dom";
import { renderApp } from "@/mocks/test-utils";
import ConnectClient from "@/pages/ConnectClient";

function mount() {
  return renderApp(
    <Routes><Route path="/clients/connect" element={<ConnectClient />} /></Routes>,
    "/clients/connect",
  );
}

describe("Connect a client", () => {
  it("mints a token for the named client and reveals it once", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    expect(await screen.findByText(/^bur_/)).toBeInTheDocument();
    expect(screen.getByText(/store this token now/i)).toBeInTheDocument();
  });

  it("shows the install command containing the client name", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    expect(await screen.findByText(/burrow connect/i)).toHaveTextContent(/--name edge-01/);
  });

  it("shows the relay server endpoint from connect-info in the install command", async () => {
    mount();
    await userEvent.type(screen.getByLabelText(/client name/i), "edge-01");
    await userEvent.click(screen.getByRole("button", { name: /generate token/i }));
    await screen.findByText(/^bur_/);
    expect(await screen.findByText("relay.example.com:7000")).toBeInTheDocument();
  });
});
