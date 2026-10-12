import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
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

  describe("keeps up with the clock", () => {
    // The clock and the timers both: the tick is an interval.
    beforeEach(() => {
      vi.useRealTimers();
      vi.useFakeTimers();
      vi.setSystemTime(new Date(NOW));
    });

    it("'just now' becomes '1 minute ago' by itself, without any other update", () => {
      render(<RelativeTime value="2026-10-06T19:46:26Z" />);
      const time = screen.getByText("just now");
      act(() => { vi.advanceTimersByTime(60_000); });
      expect(time).toHaveTextContent("1 minute ago");
      act(() => { vi.advanceTimersByTime(60_000); });
      expect(time).toHaveTextContent("2 minutes ago");
      // The exact time does not move.
      expect(time).toHaveAttribute("title", "06 Oct 2026, 19:46:26 UTC");
    });

    it("the relative time on hover of a record keeps up as well", () => {
      render(<RelativeTime value="2026-10-06T19:46:26Z" absolute />);
      const time = screen.getByText("06 Oct 2026, 19:46:26 UTC");
      expect(time).toHaveAttribute("title", "just now");
      act(() => { vi.advanceTimersByTime(60_000); });
      expect(time).toHaveAttribute("title", "1 minute ago");
    });

    it("every instance shares one interval, which stops when the last one unmounts", () => {
      expect(vi.getTimerCount()).toBe(0);
      const first = render(<RelativeTime value="2026-10-06T19:46:26Z" />);
      expect(vi.getTimerCount()).toBe(1);
      const second = render(<RelativeTime value="2026-10-06T19:40:00Z" />);
      const third = render(<RelativeTime value="2026-10-01T00:00:00Z" absolute />);
      expect(vi.getTimerCount()).toBe(1);
      first.unmount();
      second.unmount();
      expect(vi.getTimerCount()).toBe(1);
      // The one that is left is still told.
      act(() => { vi.advanceTimersByTime(60_000); });
      expect(third.container.querySelector("time")).toHaveAttribute("title", "5 days ago");
      third.unmount();
      expect(vi.getTimerCount()).toBe(0);
      // And it starts again with the next one.
      const again = render(<RelativeTime value="2026-10-06T19:47:00Z" />);
      expect(vi.getTimerCount()).toBe(1);
      again.unmount();
      expect(vi.getTimerCount()).toBe(0);
    });

    it("where there is no time there is nothing to keep up with: no interval", () => {
      const none = render(<RelativeTime value={null} />);
      const bad = render(<RelativeTime value="not-a-date" />);
      expect(vi.getTimerCount()).toBe(0);
      none.unmount();
      bad.unmount();
    });

    it("a time read between two ticks is measured against the clock, not against the last tick", () => {
      const first = render(<RelativeTime value="2026-10-06T19:00:00Z" />);
      // 40 seconds into the minute something happens and is shown: it is not "in the future".
      act(() => { vi.advanceTimersByTime(40_000); });
      const second = render(<RelativeTime value="2026-10-06T19:47:26Z" />);
      expect(second.container.querySelector("time")).toHaveTextContent("just now");
      first.unmount();
      second.unmount();
    });
  });

  // A record keeps its exact times; how long ago that was is on hover.
  it("absolute: the UTC time as the text, the relative time on hover", () => {
    render(<RelativeTime value="2026-10-03T19:46:56Z" absolute />);
    const time = screen.getByText("03 Oct 2026, 19:46:56 UTC");
    expect(time).toHaveAttribute("title", "3 days ago");
    expect(time).toHaveAttribute("datetime", "2026-10-03T19:46:56Z");
  });
});
