import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DeleteUserConfirm } from "./DeleteUserConfirm";

describe("DeleteUserConfirm", () => {
  it("names the user and what is lost, and confirms or cancels", async () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(<DeleteUserConfirm email="bob@acme.io" pending={false} onConfirm={onConfirm} onCancel={onCancel} />);
    const dialog = await screen.findByRole("dialog", { name: "Delete user?" });
    expect(dialog).toHaveTextContent("bob@acme.io will lose access immediately, and their tokens and services are removed. This cannot be undone.");
    await userEvent.click(screen.getByRole("button", { name: "Delete user" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("cannot be confirmed twice while pending", async () => {
    render(<DeleteUserConfirm email="bob@acme.io" pending onConfirm={() => {}} onCancel={() => {}} />);
    expect(await screen.findByRole("button", { name: "Delete user" })).toBeDisabled();
  });
});
