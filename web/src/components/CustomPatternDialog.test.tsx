import { describe, it, expect, vi } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { renderApp } from "@/mocks/test-utils";
import { server } from "@/mocks/server";
import { CustomPatternDialog } from "@/components/CustomPatternDialog";

describe("CustomPatternDialog: a late answer after the dialog was closed", () => {
  it("is dropped, though the dialog stays mounted", async () => {
    let release!: () => void;
    const held = new Promise<void>((r) => { release = r; });
    let answered = false;
    server.use(http.post("/api/v1/guardrails/preview", async () => {
      await held;
      answered = true;
      return HttpResponse.json({ matches: [{ start: 0, end: 1, text: "a" }], truncated: false });
    }));
    // The parent keeps the dialog mounted on close, as a test or a slow unmount would.
    const onClose = vi.fn();
    renderApp(<CustomPatternDialog takenIds={[]} onSave={async () => {}} onClose={onClose} />);
    await userEvent.type(await screen.findByLabelText("Pattern"), "a");
    await userEvent.type(screen.getByLabelText("Sample text"), "a");
    await userEvent.click(screen.getByRole("button", { name: "Test" }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    release();
    await waitFor(() => expect(answered).toBe(true));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByText("1 match")).toBeNull();
  });
});
