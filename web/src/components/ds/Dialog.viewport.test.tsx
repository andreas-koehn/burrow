import { useState } from "react";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { createPortal } from "react-dom";
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

  it("returns focus to the opener even when a child autofocuses on mount", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button onClick={() => setOpen(true)}>Open</button>
          <Dialog open={open} title="t" onOpenChange={setOpen}><input autoFocus aria-label="Query" /></Dialog>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Open" });
    opener.focus();
    fireEvent.click(opener);
    // autoFocus is applied at commit, before any effect of the dialog runs.
    expect(screen.getByLabelText("Query")).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(opener).toHaveFocus();
  });

  it("returns focus to the page opener when a chained dialog closes", async () => {
    // Create-then-reveal: A's submit button closes A and opens B, then
    // unmounts with A, so B's own opener is gone by the time B closes.
    function Harness() {
      const [a, setA] = useState(false);
      const [b, setB] = useState(false);
      return (
        <>
          <button onClick={() => setA(true)}>New key</button>
          <Dialog
            open={a}
            title="Create"
            onOpenChange={setA}
            footer={<button onClick={() => { setA(false); setB(true); }}>Create</button>}
          >
            <p>Name it</p>
          </Dialog>
          <Dialog open={b} title="Reveal" onOpenChange={setB} footer={<button onClick={() => setB(false)}>Done</button>}>
            <p>secret</p>
          </Dialog>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "New key" });
    opener.focus();
    fireEvent.click(opener);
    const create = screen.getByRole("button", { name: "Create" });
    await waitFor(() => expect(create).toHaveFocus());
    fireEvent.click(create);
    const done = screen.getByRole("button", { name: "Done" });
    await waitFor(() => expect(done).toHaveFocus());
    expect(create.isConnected).toBe(false);
    fireEvent.click(done);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(opener).toHaveFocus();
  });

  it("does not jump to an earlier opener once the user has interacted with the page", async () => {
    // Dialog A hands focus back to "New service". Later an unrelated dialog
    // whose own opener is gone closes: focus must not jump to that button.
    function Harness() {
      const [a, setA] = useState(false);
      const [b, setB] = useState(false);
      return (
        <>
          <button onClick={() => setA(true)}>New service</button>
          <p>page text</p>
          {!b && <button onClick={() => setB(true)}>Delete row</button>}
          <Dialog open={a} title="Create" onOpenChange={setA} footer={<button onClick={() => setA(false)}>Cancel</button>}>
            <p>Name it</p>
          </Dialog>
          <Dialog open={b} title="Confirm" onOpenChange={setB} footer={<button onClick={() => setB(false)}>Done</button>}>
            <p>Sure?</p>
          </Dialog>
        </>
      );
    }
    render(<Harness />);
    const first = screen.getByRole("button", { name: "New service" });
    first.focus();
    fireEvent.click(first);
    const cancel = screen.getByRole("button", { name: "Cancel" });
    await waitFor(() => expect(cancel).toHaveFocus());
    fireEvent.click(cancel);
    await waitFor(() => expect(first).toHaveFocus());

    fireEvent.pointerDown(screen.getByText("page text"));
    const rowButton = screen.getByRole("button", { name: "Delete row" });
    rowButton.focus();
    fireEvent.click(rowButton);
    const done = screen.getByRole("button", { name: "Done" });
    await waitFor(() => expect(done).toHaveFocus());
    expect(rowButton.isConnected).toBe(false);
    const focusSpy = vi.spyOn(first, "focus");
    fireEvent.click(done);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(focusSpy).not.toHaveBeenCalled();
    expect(first).not.toHaveFocus();
  });

  it("uses the chained fallback once and without scrolling", async () => {
    function Harness() {
      const [a, setA] = useState(false);
      const [b, setB] = useState(false);
      const [c, setC] = useState(false);
      return (
        <>
          <button onClick={() => setA(true)}>New key</button>
          <Dialog
            open={a}
            title="Create"
            onOpenChange={setA}
            footer={<button onClick={() => { setA(false); setB(true); }}>Create</button>}
          >
            <p>Name it</p>
          </Dialog>
          <Dialog
            open={b}
            title="Reveal"
            onOpenChange={setB}
            footer={<button onClick={() => { setB(false); setC(true); }}>Done</button>}
          >
            <p>secret</p>
          </Dialog>
          <Dialog open={c} title="Third" onOpenChange={setC} footer={<button onClick={() => setC(false)}>Finish</button>}>
            <p>more</p>
          </Dialog>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "New key" });
    opener.focus();
    fireEvent.click(opener);
    const create = screen.getByRole("button", { name: "Create" });
    await waitFor(() => expect(create).toHaveFocus());
    fireEvent.click(create);
    const done = screen.getByRole("button", { name: "Done" });
    await waitFor(() => expect(done).toHaveFocus());
    const focusSpy = vi.spyOn(opener, "focus");
    fireEvent.click(done);
    // Closing the chained dialog falls back to the page button, once.
    expect(focusSpy).toHaveBeenCalledTimes(1);
    expect(focusSpy).toHaveBeenCalledWith({ preventScroll: true });
    const finish = screen.getByRole("button", { name: "Finish" });
    await waitFor(() => expect(finish).toHaveFocus());
    fireEvent.click(finish);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    // The fallback was consumed by the second dialog.
    expect(focusSpy).toHaveBeenCalledTimes(1);
  });

  // Real order of create-then-reveal: pointerdown + click on Create, the
  // request resolves later, then A closes and B opens; B is closed by a
  // pointerdown + click on its footer button, its backdrop, or an option of
  // a portalled list interacted with in between.
  function AsyncChain() {
    const [a, setA] = useState(false);
    const [b, setB] = useState(false);
    const create = () => {
      void Promise.resolve().then(() => { setA(false); setB(true); });
    };
    return (
      <>
        <button onClick={() => setA(true)}>New key</button>
        <Dialog open={a} title="Create" onOpenChange={setA} footer={<button onClick={create}>Create</button>}>
          <p>Name it</p>
        </Dialog>
        <Dialog open={b} title="Reveal" onOpenChange={setB} footer={<button onClick={() => setB(false)}>Done</button>}>
          <p>secret</p>
        </Dialog>
        {b && createPortal(<div role="listbox"><div role="option" aria-selected="false">Portalled</div></div>, document.body)}
      </>
    );
  }
  const press = (el: Element) => {
    fireEvent.pointerDown(el);
    fireEvent.mouseDown(el);
    fireEvent.click(el);
  };
  async function openReveal() {
    const view = render(<AsyncChain />);
    const opener = screen.getByRole("button", { name: "New key" });
    opener.focus();
    press(opener);
    const create = screen.getByRole("button", { name: "Create" });
    await waitFor(() => expect(create).toHaveFocus());
    press(create);
    const done = await screen.findByRole("button", { name: "Done" });
    await waitFor(() => expect(done).toHaveFocus());
    expect(create.isConnected).toBe(false);
    return { opener, done, container: view.container };
  }

  it("async chain: closing the reveal dialog via its footer button returns focus to the page button", async () => {
    const { opener, done } = await openReveal();
    press(done);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(opener).toHaveFocus();
  });

  it("async chain: closing the reveal dialog via the backdrop returns focus to the page button", async () => {
    const { opener, container } = await openReveal();
    press(container.querySelector(".dialog-backdrop")!);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(opener).toHaveFocus();
  });

  // A real browser moves focus to the nearest focusable ancestor on a backdrop
  // mousedown: the shell's <main tabIndex={-1}>, which wraps the open dialog.
  it("async chain: a backdrop press that focuses the dialog's focusable ancestor keeps the chain", async () => {
    const host = document.createElement("main");
    host.tabIndex = -1;
    document.body.appendChild(host);
    const view = render(<AsyncChain />, { container: host });
    const opener = screen.getByRole("button", { name: "New key" });
    opener.focus();
    press(opener);
    const create = screen.getByRole("button", { name: "Create" });
    await waitFor(() => expect(create).toHaveFocus());
    press(create);
    const done = await screen.findByRole("button", { name: "Done" });
    await waitFor(() => expect(done).toHaveFocus());
    const backdrop = view.container.querySelector(".dialog-backdrop")!;
    fireEvent.pointerDown(backdrop);
    host.focus();
    fireEvent.click(backdrop);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(opener).toHaveFocus();
    view.unmount();
    host.remove();
  });

  it("async chain: a portalled list used inside the reveal dialog does not break the chain", async () => {
    const { opener, done } = await openReveal();
    fireEvent.pointerDown(screen.getByRole("option", { name: "Portalled" }));
    press(done);
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(opener).toHaveFocus();
  });

  it("skips a disconnected opener without throwing", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return (
        <>
          {!open && <button onClick={() => setOpen(true)}>Open</button>}
          <button>Other</button>
          <Dialog open={open} title="t" onOpenChange={setOpen}><input aria-label="Name" /></Dialog>
        </>
      );
    }
    render(<Harness />);
    const opener = screen.getByRole("button", { name: "Open" });
    opener.focus();
    fireEvent.click(opener);
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveFocus());
    expect(opener.isConnected).toBe(false);
    const focusSpy = vi.spyOn(opener, "focus");
    expect(() => fireEvent.keyDown(document, { key: "Escape" })).not.toThrow();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(focusSpy).not.toHaveBeenCalled();
  });
});
