import { useState } from "react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { Dialog } from "./Dialog";

// jsdom does not load CSS files — inject the dialog rules so getComputedStyle
// reflects the actual stylesheet values.
let styleEl: HTMLStyleElement;

beforeEach(() => {
  styleEl = document.createElement("style");
  styleEl.textContent = `
    .dialog {
      position: relative;
      max-height: calc(100vh - 32px);
      display: flex;
      flex-direction: column;
    }
    .dialog-body {
      overflow-y: auto;
      flex: 1 1 auto;
      min-height: 0;
    }
    .dialog-backdrop { position: fixed; inset: 0; background: oklch(0 0 0 / 0.5); }
  `;
  document.head.appendChild(styleEl);
});

afterEach(() => {
  styleEl.remove();
});

describe("Dialog viewport overflow", () => {
  it("applies a computed max-height so tall content can scroll within the viewport", () => {
    const { getByRole } = render(
      <Dialog open title="Tall" onOpenChange={() => {}}>
        <div style={{ height: 5000 }}>tall content</div>
      </Dialog>,
    );
    const dialog = getByRole("dialog");
    const cs = window.getComputedStyle(dialog);
    expect(cs.maxHeight).toMatch(/vh|calc/);
  });

  it("dialog body is the scroll container, not the dialog root", () => {
    const { getByRole } = render(
      <Dialog open title="Tall" onOpenChange={() => {}}>
        <div className="dialog-body-probe">x</div>
      </Dialog>,
    );
    const dialog = getByRole("dialog");
    const body = dialog.querySelector(".dialog-body") as HTMLElement;
    expect(body).not.toBeNull();
    const bodyCs = window.getComputedStyle(body);
    expect(bodyCs.overflowY).toBe("auto");
  });
});

describe("Dialog backdrop scrim (D-1/L-14)", () => {
  it("backdrop scrim is a fixed full-viewport layer (D-1/L-14)", () => {
    const { container } = render(<Dialog open title="x" onOpenChange={() => {}} />);
    const scrim = container.querySelector(".dialog-backdrop") as HTMLElement;
    const cs = window.getComputedStyle(scrim);
    expect(cs.position).toBe("fixed");
    expect(cs.inset === "0px" || cs.top === "0px").toBe(true);
    expect(cs.background).toContain("0.5");
  });
});

describe("Dialog size, close button, focus (F2/U2)", () => {
  it("defaults to size-sm and applies size-md / size-lg", () => {
    const { getByRole, rerender } = render(<Dialog open title="t" onOpenChange={() => {}} />);
    expect(getByRole("dialog").className).toContain("size-sm");
    rerender(<Dialog open title="t" size="md" onOpenChange={() => {}} />);
    expect(getByRole("dialog").className).toContain("size-md");
    rerender(<Dialog open title="t" size="lg" onOpenChange={() => {}} />);
    expect(getByRole("dialog").className).toContain("size-lg");
  });

  it("renders a close button that requests close", () => {
    const onOpenChange = vi.fn();
    render(<Dialog open title="t" onOpenChange={onOpenChange} />);
    fireEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("omits the close button when the dialog cannot be dismissed", () => {
    render(<Dialog open title="t" />);
    expect(screen.queryByRole("button", { name: "Close dialog" })).toBeNull();
  });

  it("puts initial focus on the first body field, not the close button", async () => {
    render(
      <Dialog open title="t" onOpenChange={() => {}} footer={<button>Save</button>}>
        <input aria-label="Name" />
      </Dialog>,
    );
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
  });

  it("falls back to the first footer button when the body has no field", async () => {
    render(
      <Dialog open title="t" onOpenChange={() => {}} footer={<><button>Cancel</button><button>Delete</button></>}>
        <p>Sure?</p>
      </Dialog>,
    );
    await waitFor(() => expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus());
  });

  it("returns focus to the opener when it closes", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button onClick={() => setOpen(true)}>Open</button>
          <Dialog open={open} title="t" onOpenChange={setOpen}><input aria-label="Name" /></Dialog>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Open" });
    opener.focus();
    fireEvent.click(opener);
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(opener).toHaveFocus());
  });
});
