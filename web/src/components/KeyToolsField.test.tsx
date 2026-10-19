import { describe, it, expect } from "vitest";
import { useState } from "react";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderApp } from "@/mocks/test-utils";
import { KeyToolsField } from "@/components/KeyToolsField";

function Harness({ initial = [] as string[], onState }: { initial?: string[]; onState?: (v: { tools: string[]; readOnly: boolean }) => void }) {
  const [tools, setTools] = useState(initial);
  const [readOnly, setReadOnly] = useState(false);
  onState?.({ tools, readOnly });
  return <KeyToolsField idPrefix="t" value={tools} onChange={setTools} readOnly={readOnly} onReadOnlyChange={setReadOnly} />;
}

const chips = () => {
  const list = screen.queryByRole("list", { name: "Allowed tools" });
  return list ? within(list).getAllByRole("listitem").map((li) => li.textContent?.replace("Remove", "").trim()) : [];
};

describe("KeyToolsField", () => {
  it("says that no tools means none", () => {
    renderApp(<Harness />, "/gateway/keys");
    expect(screen.getByText(/No tools selected\. A key without tools cannot call any tool\./)).toBeInTheDocument();
    expect(chips()).toEqual([]);
  });

  it("adds every tool of a source from the picker as src__*", async () => {
    renderApp(<Harness />, "/gateway/keys");
    await userEvent.click(await screen.findByRole("combobox", { name: "Tool source" }));
    await userEvent.click(await screen.findByRole("option", { name: "github" }));
    await userEvent.click(screen.getByRole("button", { name: "Add all tools of source" }));
    expect(chips()).toEqual(["github__*"]);
    expect(screen.queryByText(/No tools selected/)).toBeNull();
  });

  it("adds a free pattern, ignores a duplicate, and removes a chip", async () => {
    renderApp(<Harness initial={["github__*"]} />, "/gateway/keys");
    const input = screen.getByLabelText("Tool pattern");
    await userEvent.type(input, "files__read_file{Enter}");
    await userEvent.type(input, "files__read_file");
    await userEvent.click(screen.getByRole("button", { name: "Add pattern" }));
    expect(chips()).toEqual(["github__*", "files__read_file"]);
    await userEvent.click(screen.getByRole("button", { name: "Remove github__*" }));
    expect(chips()).toEqual(["files__read_file"]);
  });

  it("refuses a pattern that is not <source>__<tool> or <source>__*", async () => {
    renderApp(<Harness />, "/gateway/keys");
    const input = screen.getByLabelText("Tool pattern");
    for (const bad of ["read_file", "GitHub__x", "github__", "git__hub__*x y", "src__read_*", "burrow__x", "v1__x"]) {
      await userEvent.clear(input);
      await userEvent.type(input, bad);
      await userEvent.click(screen.getByRole("button", { name: "Add pattern" }));
      expect(await screen.findByRole("alert")).toHaveTextContent(/<source>__<tool>/);
    }
    expect(chips()).toEqual([]);
  });

  it("toggles the read-only switch", async () => {
    let state = { tools: [] as string[], readOnly: false };
    renderApp(<Harness onState={(s) => { state = s; }} />, "/gateway/keys");
    const sw = screen.getByRole("switch", { name: "Read-only tools only" });
    expect(sw).toHaveAttribute("aria-checked", "false");
    await userEvent.click(sw);
    expect(state.readOnly).toBe(true);
  });
});
