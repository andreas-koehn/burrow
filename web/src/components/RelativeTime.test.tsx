import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { RelativeTime } from "./RelativeTime";

const NOW = "2026-10-06T19:46:56Z";

describe("RelativeTime", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(NOW));
  });
  afterEach(() => vi.useRealTimers());

  it("says how long ago, with the absolute UTC time on hover", () => {
    render(<RelativeTime value="2026-10-03T19:46:56Z" />);
    const time = screen.getByText("3 days ago");
    expect(time.tagName).toBe("TIME");
    expect(time).toHaveAttribute("datetime", "2026-10-03T19:46:56Z");
    expect(time).toHaveAttribute("title", "03 Oct 2026, 19:46:56 UTC");
  });

  it.each([
    ["2026-10-06T19:46:30Z", "just now"],
    ["2026-10-06T19:41:56Z", "5 minutes ago"],
    ["2026-10-06T17:00:00Z", "2 hours ago"],
    ["2026-10-05T19:46:56Z", "1 day ago"],
    ["2026-08-01T12:00:00Z", "01 Aug 2026"],
  ])("%s reads %s", (value, text) => {
    render(<RelativeTime value={value} />);
    expect(screen.getByText(text)).toHaveAttribute("title", expect.stringMatching(/ UTC$/));
  });

  it("writes a time in the future as the absolute time", () => {
    render(<RelativeTime value="2026-10-07T08:00:00Z" />);
    const time = screen.getByText("07 Oct 2026, 08:00:00 UTC");
    expect(time).toHaveAttribute("title", "07 Oct 2026, 08:00:00 UTC");
  });

  it("is a dash without a time, and no <time> element", () => {
    const { container } = render(<RelativeTime value={null} />);
    expect(container).toHaveTextContent("—");
    expect(container.querySelector("time")).toBeNull();
  });

  it("says what the caller wants instead of the dash", () => {
    const { container } = render(<RelativeTime value={null} empty="never" />);
    expect(container).toHaveTextContent("never");
  });

  it("treats what is no time as no time", () => {
    const { container } = render(<RelativeTime value="not-a-date" />);
    expect(container).toHaveTextContent("—");
    expect(container.querySelector("time")).toBeNull();
  });

  // A record keeps its exact times; how long ago that was is on hover.
  it("absolute: the UTC time as the text, the relative time on hover", () => {
    render(<RelativeTime value="2026-10-03T19:46:56Z" absolute />);
    const time = screen.getByText("03 Oct 2026, 19:46:56 UTC");
    expect(time).toHaveAttribute("title", "3 days ago");
    expect(time).toHaveAttribute("datetime", "2026-10-03T19:46:56Z");
  });
});
