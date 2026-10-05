import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ServiceUrl } from "./ServiceUrl";

describe("ServiceUrl", () => {
  it("shows the path and copies the full URL", async () => {
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue();
    render(<ServiceUrl slug="k7p2qx" url="https://tunnels.example.com/svc/k7p2qx/" />);
    expect(screen.getByText("/svc/k7p2qx/")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy URL https://tunnels.example.com/svc/k7p2qx/" }));
    expect(writeText).toHaveBeenCalledWith("https://tunnels.example.com/svc/k7p2qx/");
    writeText.mockRestore();
  });

  it("falls back to the dashboard origin when the relay reports no URL", () => {
    render(<ServiceUrl slug="k7p2qx" />);
    expect(screen.getByText("/svc/k7p2qx/")).toHaveAttribute("title", `${window.location.origin}/svc/k7p2qx/`);
  });

  it("shows the path of the reported URL when the slug is empty", () => {
    render(<ServiceUrl slug="" url="https://tunnels.example.com/svc/k7p2qx/" />);
    expect(screen.getByText("/svc/k7p2qx/")).toBeInTheDocument();
    expect(screen.queryByText("/svc//")).toBeNull();
  });

  it("renders a dash when there is neither slug nor URL", () => {
    render(<ServiceUrl slug="" />);
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByRole("button")).toBeNull();
  });
});
