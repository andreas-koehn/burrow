import { describe, it, expect, afterEach, vi } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { InstallLines } from "./InstallLines";

const ORIGIN = "https://b.example.com";
const lineTexts = () => Array.from(document.querySelectorAll(".install-line pre code")).map((c) => c.textContent);

function fakeBrowser(platform: string, userAgent: string) {
  vi.spyOn(window.navigator, "platform", "get").mockReturnValue(platform);
  vi.spyOn(window.navigator, "userAgent", "get").mockReturnValue(userAgent);
}

describe("InstallLines", () => {
  afterEach(() => vi.restoreAllMocks());

  it("offers Linux, macOS and Windows in a radiogroup named Operating system", () => {
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    const group = screen.getByRole("radiogroup", { name: "Operating system" });
    expect(within(group).getAllByRole("radio").map((r) => r.textContent)).toEqual(["Linux", "macOS", "Windows"]);
  });

  it.each([
    ["Win32", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "Windows"],
    ["MacIntel", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", "macOS"],
    ["Linux x86_64", "Mozilla/5.0 (X11; Linux x86_64)", "Linux"],
  ])("preselects the visitor's system (%s)", (platform, ua, label) => {
    fakeBrowser(platform, ua);
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    expect(screen.getByRole("radio", { name: label })).toHaveAttribute("aria-checked", "true");
  });

  it("shows three numbered lines as selectable text", () => {
    fakeBrowser("Linux x86_64", "");
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    const items = within(screen.getByRole("list", { name: "Commands" })).getAllByRole("listitem");
    expect(items.map((li) => li.querySelector(".install-line-label")?.textContent)).toEqual(["1. Install", "2. Sign in", "3. Run"]);
    expect(lineTexts()).toEqual([
      "curl -fsSL https://b.example.com/install.sh | sh",
      "burrow login b.example.com",
      "burrow http 3000",
    ]);
  });

  it("switching the system changes the first line only", async () => {
    fakeBrowser("Linux x86_64", "");
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    const before = lineTexts();
    await userEvent.click(screen.getByRole("radio", { name: "Windows" }));
    const after = lineTexts();
    expect(after[0]).toBe("irm https://b.example.com/install.ps1 | iex");
    expect(after.slice(1)).toEqual(before.slice(1));
    await userEvent.click(screen.getByRole("radio", { name: "macOS" }));
    expect(lineTexts()).toEqual(before);
  });

  it.each([
    ["Copy install command", 0, "Install command copied."],
    ["Copy sign-in command", 1, "Sign-in command copied."],
    ["Copy run command", 2, "Run command copied."],
  ])("'%s' copies exactly the line shown and says so without moving the focus", async (name, at, said) => {
    fakeBrowser("Win32", "");
    const user = userEvent.setup();
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    const button = screen.getByRole("button", { name });
    button.focus();
    await user.keyboard("{Enter}");
    expect(await navigator.clipboard.readText()).toBe(lineTexts()[at]);
    expect(await screen.findByText(said)).toHaveAttribute("role", "status");
    expect(button).toHaveFocus();
  });

  it("says so when the browser refuses to copy", async () => {
    const user = userEvent.setup();
    vi.spyOn(navigator.clipboard, "writeText").mockRejectedValue(new Error("denied"));
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    await user.click(screen.getByRole("button", { name: "Copy run command" }));
    expect(await screen.findByText("Could not copy. Select the text instead.")).toHaveAttribute("role", "status");
  });

  it("says that the sign-in is approved in the browser, points to a token for machines without one, and shows no token", () => {
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    const hint = screen.getByText(/compare the code/i);
    expect(hint).toHaveTextContent("It opens this dashboard in your browser: compare the code with the one in your terminal and approve.");
    expect(hint).toHaveTextContent("Or sign in with a token: add --token - and paste one from Clients, tab Tokens.");
    expect(document.querySelector(".install-lines")!.textContent).not.toMatch(/asks for a client token/);
    expect(screen.getByRole("link", { name: "Clients, tab Tokens" })).toHaveAttribute("href", "/clients?tab=tokens");
    expect(document.querySelector(".install-lines")!.textContent).not.toMatch(/bur_/);
  });

  it("can show a part of the lines; without the install line there is no system to choose", () => {
    fakeBrowser("Linux x86_64", "");
    const first = renderApp(<InstallLines relayOrigin={ORIGIN} lines={["install", "login"]} />);
    expect(screen.getByRole("radiogroup", { name: "Operating system" })).toBeInTheDocument();
    expect(lineTexts()).toEqual(["curl -fsSL https://b.example.com/install.sh | sh", "burrow login b.example.com"]);
    first.unmount();
    renderApp(<InstallLines relayOrigin={ORIGIN} lines={["run"]} />);
    expect(screen.queryByRole("radiogroup")).toBeNull();
    expect(lineTexts()).toEqual(["burrow http 3000"]);
    expect(screen.getByRole("button", { name: "Copy run command" })).toBeInTheDocument();
  });

  it("warns when the dashboard is not served over https", () => {
    renderApp(<InstallLines relayOrigin="http://localhost:8080" />);
    expect(screen.getByText(/not served over HTTPS/)).toBeInTheDocument();
    expect(lineTexts()[0]).toContain("http://localhost:8080/install.sh");
  });

  it("has no such warning on https", () => {
    renderApp(<InstallLines relayOrigin={ORIGIN} />);
    expect(screen.queryByText(/not served over HTTPS/)).toBeNull();
  });
});
