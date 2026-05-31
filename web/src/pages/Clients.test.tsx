import { describe, it, expect } from "vitest";
import { screen, within } from "@testing-library/react";
import { renderApp } from "@/mocks/test-utils";
import { Routes, Route } from "react-router-dom";
import Clients from "@/pages/Clients";

describe("Clients overview", () => {
  it("lists connected clients with platform and traffic", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    expect(await screen.findByText("office-box-1")).toBeInTheDocument();
    const row = screen.getByText("office-box-1").closest("tr")!;
    expect(within(row).getByText(/linux/i)).toBeInTheDocument();
  });

  it("has a link to client detail", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    const row = (await screen.findByText("office-box-1")).closest("tr")!;
    // The "View" button link — exact text match to avoid collision with the services-count link
    const links = within(row).getAllByRole("link");
    const viewLink = links.find((l) => l.textContent?.trim() === "View");
    expect(viewLink).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });

  it("services-count badge links to client detail (P3B.1)", async () => {
    renderApp(
      <Routes><Route path="/clients" element={<Clients />} /></Routes>,
      "/clients",
    );
    await screen.findByText("office-box-1");
    const link = screen.getByRole("link", { name: /view \d+ services for office-box-1/i });
    expect(link).toHaveAttribute("href", "/clients/sess_4f7a9c0b2e81");
  });
});
