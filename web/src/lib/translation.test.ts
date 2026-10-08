import { describe, it, expect } from "vitest";
import { droppedNames, pairWords, towardMessages } from "./translation";

describe("pairWords", () => {
  it("words the four pairs", () => {
    expect(pairWords("messages-chat")).toBe("Anthropic Messages → Chat Completions");
    expect(pairWords("responses-messages")).toBe("OpenAI Responses → Anthropic Messages");
  });
  it("shows an id it does not know as it is, also one that names an object member", () => {
    for (const id of ["future-pair", "constructor", "toString", "__proto__", "hasOwnProperty"]) {
      expect(pairWords(id)).toBe(id);
    }
  });
});

describe("towardMessages", () => {
  it("is true when any pair asks an Anthropic-format provider", () => {
    expect(towardMessages(["responses-chat", "responses-messages"])).toBe(true);
    expect(towardMessages(["responses-chat"])).toBe(false);
    expect(towardMessages([])).toBe(false);
    expect(towardMessages(undefined)).toBe(false);
  });
});

describe("droppedNames", () => {
  it("reads a final more as the mark of a cut list", () => {
    expect(droppedNames(["store", "more"])).toEqual({ names: ["store"], more: true });
    expect(droppedNames(["more", "store"])).toEqual({ names: ["more", "store"], more: false });
    expect(droppedNames(null)).toEqual({ names: [], more: false });
  });
});
