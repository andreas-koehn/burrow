export interface Edges { top: number; bottom: number; left: number; right: number }
export interface Size { width: number; height: number }

const GAP = 4;
const PAD = 8;

/**
 * Viewport-relative position for a menu anchored to a trigger. Opens below,
 * flips above when it would leave the viewport and there is room above, and
 * is clamped horizontally so it never leaves the viewport.
 */
export function placeMenu(
  trigger: Edges,
  menu: Size,
  viewport: Size,
  align: "left" | "right",
): { top: number; left: number } {
  let left = align === "right" ? trigger.right - menu.width : trigger.left;
  left = Math.max(PAD, Math.min(left, viewport.width - menu.width - PAD));

  let top = trigger.bottom + GAP;
  const above = trigger.top - GAP - menu.height;
  if (top + menu.height > viewport.height - PAD && above >= PAD) top = above;

  return { top, left };
}
