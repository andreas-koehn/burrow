import "@testing-library/jest-dom/vitest";
import { vi, afterAll, afterEach, beforeAll } from "vitest";
import { server } from "@/mocks/server";
import { resetDb } from "@/mocks/db";

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => { server.resetHandlers(); resetDb(); });
afterAll(() => server.close());

// jsdom does not implement matchMedia — provide a minimal stub.
// Tests that need specific dark/light behaviour should override this per-test.
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: vi.fn(),
    removeListener: vi.fn(),
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
    dispatchEvent: vi.fn(),
  })),
});

// jsdom does not implement pointer capture; sonner calls it when a toast (or a
// button inside one) is pressed.
for (const name of ["setPointerCapture", "releasePointerCapture"] as const) {
  if (!Element.prototype[name]) Element.prototype[name] = () => {};
}
if (!Element.prototype.hasPointerCapture) Element.prototype.hasPointerCapture = () => false;
