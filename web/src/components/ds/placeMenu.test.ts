import { describe, it, expect } from "vitest";
import { placeMenu } from "./placeMenu";

const vp = { width: 1000, height: 600 };
const menu = { width: 200, height: 160 };

describe("placeMenu", () => {
  it("opens below the trigger, right-aligned", () => {
    const t = { top: 100, bottom: 128, left: 700, right: 728 };
    expect(placeMenu(t, menu, vp, "right")).toEqual({ top: 132, left: 528 });
  });
  it("opens below the trigger, left-aligned", () => {
    const t = { top: 100, bottom: 128, left: 300, right: 328 };
    expect(placeMenu(t, menu, vp, "left")).toEqual({ top: 132, left: 300 });
  });
  it("flips above when there is no room below", () => {
    const t = { top: 500, bottom: 528, left: 700, right: 728 };
    expect(placeMenu(t, menu, vp, "right")).toEqual({ top: 336, left: 528 });
  });
  it("stays below when it fits neither below nor above", () => {
    const t = { top: 100, bottom: 128, left: 700, right: 728 };
    const tall = { width: 200, height: 580 };
    expect(placeMenu(t, tall, vp, "right").top).toBe(132);
  });
  it("clamps inside the left and right viewport edges", () => {
    const nearLeft = { top: 100, bottom: 128, left: 20, right: 48 };
    expect(placeMenu(nearLeft, menu, vp, "right").left).toBe(8);
    const nearRight = { top: 100, bottom: 128, left: 950, right: 978 };
    expect(placeMenu(nearRight, menu, vp, "left").left).toBe(792);
  });
});
