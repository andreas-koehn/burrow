import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { FOOTER_ENTRIES, navigationFor, workspacesFor } from "@/lib/navigation";
import { workspaceFor } from "@/lib/workspace";
import { Sidebar, type SidebarProps } from "./Sidebar";

const admin = { isAdmin: true, hasAiGateway: true };
const plain = { isAdmin: false, hasAiGateway: false };

function mount(path: string, ctx = admin, over: Partial<SidebarProps> = {}) {
  const workspace = workspaceFor(path);
  const props: SidebarProps = {
    navigation: navigationFor(workspace, ctx),
    workspaces: workspacesFor(ctx),
    footer: FOOTER_ENTRIES.filter((e) => !e.adminOnly || ctx.isAdmin),
    pathname: path,
    collapsed: false,
    counts: {},
    attention: [],
    user: { email: "alice@example.com", isAdmin: ctx.isAdmin, role: ctx.isAdmin ? "admin" : "user" },
    theme: "light",
    onSearch: vi.fn(),
    onToggleTheme: vi.fn(),
    onLogout: vi.fn(),
    ...over,
  };
  const view = render(<MemoryRouter initialEntries={[path]}><Sidebar {...props} /></MemoryRouter>);
  return { props, ...view };
}

const entries = (name: string) =>
  within(screen.getByRole("navigation", { name })).getAllByRole("link").map((a) => a.getAttribute("aria-label"));

const footerZone = () => document.querySelector(".sidebar-footer") as HTMLElement;
const footerShape = () =>
  [...footerZone().querySelectorAll("a, button")].map((el) => [el.tagName, el.getAttribute("aria-label"), el.getAttribute("href")]);

describe("Sidebar", () => {
  beforeEach(() => localStorage.clear());

  it("Services workspace: its entries, with the parent current on a detail page", () => {
    mount("/services/abc");
    expect(entries("Services")).toEqual(["Overview", "Services", "Clients", "Traffic"]);
    const nav = screen.getByRole("navigation", { name: "Services" });
    const services = within(nav).getByRole("link", { name: "Services" });
    expect(services).toHaveAttribute("aria-current", "page");
    expect(services).toHaveClass("nav-item", "is-active");
    const overview = within(nav).getByRole("link", { name: "Overview" });
    expect(overview).not.toHaveAttribute("aria-current");
    expect(overview).not.toHaveClass("is-active");
    expect(within(nav).getByText("Connect")).toHaveClass("nav-group-title");
  });

  it("AI Gateway workspace: its entries, Providers current on a provider page", () => {
    mount("/gateway/providers/zai");
    expect(entries("AI Gateway")).toEqual(["Overview", "Providers", "Guardrails", "Prompt cache", "Requests", "Cost & budgets"]);
    expect(screen.getByRole("link", { name: "Providers" })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("link", { name: "Requests" })).toHaveAttribute("href", "/gateway/requests");
    expect(screen.getByRole("button", { name: "Workspace: AI Gateway" })).toBeInTheDocument();
  });

  it("Settings: a way back to the last workspace instead of the switcher", () => {
    mount("/settings");
    expect(screen.getByRole("navigation", { name: "Settings" })).toBeInTheDocument();
    const first = document.querySelector(".sidebar a") as HTMLAnchorElement;
    expect(first).toHaveAccessibleName("Back to Services");
    expect(first).toHaveAttribute("href", "/");
    expect(screen.queryByRole("button", { name: /^Workspace:/ })).toBeNull();
    expect(document.querySelector(".workspace-title")).toHaveTextContent("Settings");
  });

  it("Settings: goes back to the AI Gateway when that was the last workspace", () => {
    localStorage.setItem("burrow.lastWorkspace", "gateway");
    mount("/settings");
    expect(screen.getByRole("link", { name: "Back to AI Gateway" })).toHaveAttribute("href", "/gateway");
  });

  it("Settings: never offers a way back to a workspace the user cannot see", () => {
    localStorage.setItem("burrow.lastWorkspace", "gateway");
    mount("/settings", plain);
    expect(screen.getByRole("link", { name: "Back to Services" })).toHaveAttribute("href", "/");
  });

  it("the footer zone is identical in both workspaces", () => {
    const first = mount("/services");
    const inServices = footerShape();
    first.unmount();
    mount("/gateway/cost");
    expect(footerShape()).toEqual(inServices);
    expect(inServices).toEqual([
      ["A", "Users & roles", "/settings/users"],
      ["A", "Settings", "/settings/general"],
      ["A", "Your profile, alice@example.com", "/settings/profile"],
      ["BUTTON", "Switch to dark theme", null],
      ["BUTTON", "Log out", null],
    ]);
  });

  it("a non-admin gets the user chip but no admin shortcuts", () => {
    mount("/services", plain);
    expect(screen.queryByRole("link", { name: "Users & roles" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Settings" })).toBeNull();
    const chip = screen.getByRole("link", { name: "Your profile, alice@example.com" });
    expect(chip).toHaveClass("user-chip");
    expect(chip).toHaveTextContent("alice@example.com");
    expect(chip).toHaveTextContent("USER");
  });

  it("shows live counts and reads them out in words", () => {
    mount("/services", admin, { counts: { services: 2, clientsOnline: 1 } });
    const services = screen.getByRole("link", { name: "Services, 2" });
    expect(within(services).getByText("2")).toHaveClass("nav-count");
    const clients = screen.getByRole("link", { name: "Clients, 1 online" });
    expect(within(clients).getByText("1 on")).toHaveClass("nav-count");
  });

  it("shows no count while the figure is unknown", () => {
    mount("/services");
    expect(within(screen.getByRole("link", { name: "Services" })).queryByText(/\d/)).toBeNull();
  });

  it("collapsed: icons only, every control keeps its name", () => {
    mount("/services", admin, { collapsed: true, counts: { services: 2 } });
    expect(document.querySelector(".sidebar")).toHaveClass("is-collapsed");
    expect(entries("Services")).toEqual(["Overview", "Services, 2", "Clients", "Traffic"]);
    expect(document.querySelector(".nav-label")).toBeNull();
    expect(document.querySelector(".nav-group-title")).toBeNull();
    expect(document.querySelector(".nav-count")).toBeNull();
    expect(screen.getByRole("link", { name: "Traffic" })).toHaveAttribute("title", "Traffic");
    expect(screen.getByRole("button", { name: "Search" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Workspace: Services" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Users & roles" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Your profile, alice@example.com" })).not.toHaveTextContent("alice");
    expect(screen.getByRole("button", { name: "Log out" })).toBeInTheDocument();
  });

  it("wires search, theme and logout", async () => {
    const { props } = mount("/services");
    await userEvent.click(screen.getByRole("button", { name: "Search" }));
    await userEvent.click(screen.getByRole("button", { name: "Switch to dark theme" }));
    await userEvent.click(screen.getByRole("button", { name: "Log out" }));
    expect(props.onSearch).toHaveBeenCalledTimes(1);
    expect(props.onToggleTheme).toHaveBeenCalledTimes(1);
    expect(props.onLogout).toHaveBeenCalledTimes(1);
  });

  it("offers the dark-theme label when the theme is dark", () => {
    mount("/services", admin, { theme: "dark" });
    const toggle = screen.getByRole("button", { name: "Switch to light theme" });
    expect(toggle.getAttribute("title")).toBe(toggle.getAttribute("aria-label"));
  });

  it("the Settings shortcut carries the mark and leads to the page that resolves the notice", () => {
    mount("/", admin, { attention: ["/settings/email"] });
    const settings = within(footerZone()).getByRole("link", { name: "Settings, needs attention" });
    expect(settings).toHaveAttribute("href", "/settings/email");
    expect(within(settings).getByText("needs attention")).toHaveClass("visually-hidden");
    expect(settings.querySelector(".nav-attention")).not.toBeNull();
    expect(within(footerZone()).getByRole("link", { name: "Users & roles" }).querySelector(".nav-attention")).toBeNull();
  });

  it("keeps the mark when collapsed, in the entry's name and tooltip", () => {
    mount("/", admin, { attention: ["/settings/email"], collapsed: true });
    const settings = within(footerZone()).getByRole("link", { name: "Settings, needs attention" });
    expect(settings).toHaveAttribute("title", "Settings, needs attention");
    expect(settings.querySelector(".nav-attention")).not.toBeNull();
  });

  it("inside Settings the entry of that page carries the mark, and only that one", () => {
    mount("/settings/general", admin, { attention: ["/settings/email"], footer: [] });
    const nav = within(screen.getByRole("navigation", { name: "Settings" }));
    const email = nav.getByRole("link", { name: "Email, needs attention" });
    expect(email).toHaveAttribute("href", "/settings/email");
    expect(within(email).getByText("needs attention")).toHaveClass("visually-hidden");
    expect(document.querySelectorAll(".nav-attention")).toHaveLength(1);
    expect(nav.getByRole("link", { name: "General" })).toHaveAttribute("aria-current", "page");
  });

  it("inside Settings, collapsed, the Email entry keeps the mark in its name and tooltip", () => {
    mount("/settings/general", admin, { attention: ["/settings/email"], footer: [], collapsed: true });
    const nav = within(screen.getByRole("navigation", { name: "Settings" }));
    const email = nav.getByRole("link", { name: "Email, needs attention" });
    expect(email).toHaveAttribute("title", "Email, needs attention");
    expect(email.querySelector(".nav-attention")).not.toBeNull();
    expect(email.querySelector(".nav-label")).toBeNull();
    expect(document.querySelectorAll(".nav-attention")).toHaveLength(1);
    expect(nav.getByRole("link", { name: "General" })).toHaveAttribute("title", "General");
  });

  it("has no mark without an open relay notice", () => {
    mount("/");
    expect(within(footerZone()).getByRole("link", { name: "Settings" })).toHaveAttribute("href", "/settings/general");
    expect(document.querySelector(".nav-attention")).toBeNull();
    expect(screen.queryByRole("link", { name: /needs attention/ })).toBeNull();
  });
});
