import { describe, it, expect } from "vitest";
import {
  ACCESS_LABEL, forwardCommand, localPort, riskyPort, widenCidr, remaining, FORWARD_LOCAL_PORT,
} from "@/lib/tcpAccess";

describe("tcp access helpers", () => {
  it("names every access mode", () => {
    expect(ACCESS_LABEL).toMatchObject({ open: "Open", ip: "IP allowlist", private: "Private" });
  });

  it("reads the port out of a local address", () => {
    expect(localPort("127.0.0.1:3389")).toBe(3389);
    expect(localPort("[::1]:22")).toBe(22);
    expect(localPort("localhost")).toBeNull();
    expect(localPort("")).toBeNull();
  });

  it.each([3389, 22, 5900, 5432])("warns about port %i", (p) => {
    expect(riskyPort(`127.0.0.1:${p}`)).toBe(p);
  });
  it("does not warn about other ports", () => {
    expect(riskyPort("127.0.0.1:8080")).toBeNull();
    expect(riskyPort("")).toBeNull();
  });

  it("widens an address to its /24 or /64", () => {
    expect(widenCidr("203.0.113.7/32")).toBe("203.0.113.0/24");
    expect(widenCidr("203.0.113.7")).toBe("203.0.113.0/24");
    expect(widenCidr("2001:db8:1:2:3:4:5:6/128")).toBe("2001:db8:1:2::/64");
    expect(widenCidr("2001:db8::7/128")).toBe("2001:db8::/64");
    expect(widenCidr("::1/128")).toBe("::/64");
    expect(widenCidr("2001:db8:0:5::9/128")).toBe("2001:db8:0:5::/64");
  });
  it("leaves a range that is already as wide", () => {
    expect(widenCidr("203.0.113.0/24")).toBeNull();
    expect(widenCidr("10.0.0.0/8")).toBeNull();
    expect(widenCidr("2001:db8::/64")).toBeNull();
    expect(widenCidr("nonsense")).toBeNull();
  });

  it("writes the forward command and the default local port", () => {
    expect(FORWARD_LOCAL_PORT).toBe(13389);
    expect(forwardCommand("rdp")).toBe("burrow forward rdp 13389");
  });

  it("says how long a grant has left", () => {
    const now = Date.parse("2026-10-23T10:00:00Z");
    expect(remaining("2026-10-23T10:00:30Z", now)).toBe("less than a minute left");
    expect(remaining("2026-10-23T10:45:00Z", now)).toBe("45 min left");
    expect(remaining("2026-10-23T12:30:00Z", now)).toBe("2 h 30 min left");
    expect(remaining("2026-10-23T14:00:00Z", now)).toBe("4 h left");
    expect(remaining("2026-10-25T10:00:00Z", now)).toBe("2 d left");
    expect(remaining("2026-10-23T09:00:00Z", now)).toBe("expired");
  });
});
