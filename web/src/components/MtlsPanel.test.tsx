import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MtlsPanel } from "@/components/MtlsPanel";
import { Dialog } from "@/components/ds";

describe("MtlsPanel", () => {
  it("renders a mono textarea for the CA PEM", () => {
    render(<MtlsPanel value="" onChange={() => {}} />);
    const ta = screen.getByLabelText(/ca pem/i);
    expect(ta.tagName.toLowerCase()).toBe("textarea");
    expect(ta.className).toContain("mono");
  });

  it("Compute fingerprint calls crypto.subtle.digest and surfaces the hex", async () => {
    const fakeBuf = new Uint8Array([0xde, 0xad, 0xbe, 0xef]).buffer;
    const digestMock = vi.fn().mockResolvedValue(fakeBuf);
    Object.defineProperty(globalThis, "crypto", {
      value: { subtle: { digest: digestMock } },
      configurable: true,
    });
    render(<MtlsPanel value={"-----BEGIN CERTIFICATE-----\nMIIB...=\n-----END CERTIFICATE-----"} onChange={() => {}} />);
    await userEvent.click(screen.getByRole("button", { name: /compute fingerprint/i }));
    await waitFor(() => expect(digestMock).toHaveBeenCalledTimes(1));
    expect(digestMock.mock.calls[0]![0]).toBe("SHA-256");
    // Cross-realm Uint8Array (jsdom realm vs test realm) — verify shape, not identity.
    const arg = digestMock.mock.calls[0]![1] as { byteLength: number; BYTES_PER_ELEMENT: number };
    expect(arg.byteLength).toBeGreaterThan(0);
    expect(arg.BYTES_PER_ELEMENT).toBe(1);
    expect(await screen.findByText(/deadbeef/)).toBeInTheDocument();
  });
});

describe("MtlsPanel upload affordance (L9)", () => {
  it("offers a DS button instead of a bare file input", () => {
    render(<MtlsPanel value="" onChange={() => {}} />);
    expect(screen.getByRole("button", { name: "Upload file…" })).toBeInTheDocument();
    const input = screen.getByLabelText("Upload CA bundle");
    expect(input).toHaveAttribute("type", "file");
    expect(input.className).toContain("visually-hidden");
  });

  it("loads the picked file into the textarea value", async () => {
    const onChange = vi.fn();
    render(<MtlsPanel value="" onChange={onChange} />);
    const file = new File(["-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----"], "ca.pem", { type: "application/x-pem-file" });
    await userEvent.upload(screen.getByLabelText("Upload CA bundle"), file);
    await waitFor(() => {
      expect(onChange).toHaveBeenCalledWith(expect.stringContaining("BEGIN CERTIFICATE"));
    });
  });

  it("does not take the dialog's initial focus or a tab stop; the textarea does", async () => {
    render(
      <Dialog open title="Access">
        <MtlsPanel value="" onChange={() => {}} />
      </Dialog>,
    );
    const textarea = screen.getByLabelText(/ca pem/i);
    await waitFor(() => expect(textarea).toHaveFocus());
    expect(screen.getByLabelText("Upload CA bundle")).toHaveAttribute("tabindex", "-1");
    // The visible button is the keyboard path to the picker.
    const button = screen.getByRole("button", { name: "Upload file…" });
    expect(button).not.toHaveAttribute("tabindex", "-1");
    await userEvent.tab({ shift: true });
    expect(button).toHaveFocus();
  });

  it("opens the file picker from the visible button", async () => {
    render(<MtlsPanel value="" onChange={() => {}} />);
    const click = vi.spyOn(screen.getByLabelText("Upload CA bundle") as HTMLInputElement, "click");
    await userEvent.click(screen.getByRole("button", { name: "Upload file…" }));
    expect(click).toHaveBeenCalledTimes(1);
  });
});
