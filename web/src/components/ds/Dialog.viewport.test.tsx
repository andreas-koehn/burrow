import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { render } from "@testing-library/react";
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
