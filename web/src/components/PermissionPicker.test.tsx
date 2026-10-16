import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PermissionPicker } from "@/components/PermissionPicker";
import { ALL_PERMISSIONS } from "@/auth/permissions";

const ALL = new Set(ALL_PERMISSIONS);

describe("PermissionPicker", () => {
  it("renders one group per area, named by the area, with a checkbox per permission", () => {
    render(<PermissionPicker value={[]} onChange={() => {}} held={ALL} idPrefix="t" />);
    const ai = screen.getByRole("group", { name: "AI gateway" });
    expect(within(ai).getByRole("checkbox", { name: "ai:read:own" })).toBeInTheDocument();
    expect(within(ai).getByRole("checkbox", { name: "ai:providers:manage" })).toBeInTheDocument();
    expect(screen.getAllByRole("checkbox")).toHaveLength(ALL_PERMISSIONS.length);
    for (const name of ["Service connections", "Services", "Limits", "IP and country rules", "Cost"]) {
      expect(screen.getByRole("group", { name })).toBeInTheDocument();
    }
  });

  it("disables a permission the reader does not hold and says why", () => {
    const held = new Set(ALL_PERMISSIONS.filter((p) => p !== "users:manage"));
    render(<PermissionPicker value={[]} onChange={() => {}} held={held} idPrefix="t" />);
    const box = screen.getByRole("checkbox", { name: "users:manage" });
    expect(box).toBeDisabled();
    expect(box).toHaveAccessibleDescription("You do not hold this.");
    expect(screen.getByRole("checkbox", { name: "users:read" })).toBeEnabled();
  });

  it("adds and removes a permission, keeping ALL_PERMISSIONS order", async () => {
    const onChange = vi.fn();
    const { rerender } = render(
      <PermissionPicker value={["audit:read", "tunnels:read:own"]} onChange={onChange} held={ALL} idPrefix="t" />,
    );
    await userEvent.click(screen.getByRole("checkbox", { name: "tunnels:manage:own" }));
    expect(onChange).toHaveBeenLastCalledWith(["tunnels:read:own", "tunnels:manage:own", "audit:read"]);
    await userEvent.click(screen.getByRole("checkbox", { name: "audit:read" }));
    expect(onChange).toHaveBeenLastCalledWith(["tunnels:read:own"]);
    rerender(<PermissionPicker value={["audit:read"]} onChange={onChange} held={ALL} idPrefix="t" />);
    expect(screen.getByRole("checkbox", { name: "audit:read" })).toBeChecked();
  });
});
