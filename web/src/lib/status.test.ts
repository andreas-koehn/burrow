import { describe, it, expect } from "vitest";
import { STATE_BADGE, STATE_LABEL, clientsOnline, providerState, serviceState, statusLabel, type State } from "./status";

describe("clientsOnline", () => {
  const session = (user_id: string, token_name: string) => ({ user_id, token_name });
  it("counts clients, not sessions", () => {
    expect(clientsOnline([])).toBe(0);
    expect(clientsOnline([session("u1", "office-box-1")])).toBe(1);
    // One token connected twice is one client.
    expect(clientsOnline([session("u1", "office-box-1"), session("u1", "office-box-1"), session("u1", "laptop")])).toBe(2);
  });
  it("two owners may give their tokens the same name", () => {
    expect(clientsOnline([session("u1", "laptop"), session("u2", "laptop")])).toBe(2);
    // No pair of user and name reads like another one.
    expect(clientsOnline([session("u1", "a b"), session("u1 a", "b")])).toBe(2);
  });
});

describe("statusLabel", () => {
  it.each([
    ["Connected", "connected"],
    ["Offline", "offline"],
    ["Degraded", "degraded"],
    ["closed_clean", "closed clean"],
    ["idle", "idle"],
    ["", ""],
    ["Some_New_State", "some new state"],
    [undefined, ""],
    [null, ""],
  ])("%j → %j", (raw, want) => {
    expect(statusLabel(raw as string)).toBe(want);
  });
});

describe("serviceState", () => {
  it("uses the relay's status", () => {
    expect(serviceState({ status: "never_connected", connected: false })).toBe("never_connected");
    expect(serviceState({ status: "offline", connected: false })).toBe("offline");
    expect(serviceState({ status: "online", connected: true })).toBe("online");
  });
  it("falls back to connected on an older relay", () => {
    expect(serviceState({ status: undefined as never, connected: true })).toBe("online");
    expect(serviceState({ status: undefined as never, connected: false })).toBe("offline");
  });
  it("falls back on a status it does not know", () => {
    expect(serviceState({ status: "paused" as never, connected: true })).toBe("online");
  });
});

describe("providerState", () => {
  it("uses state", () => {
    expect(providerState({ state: "ready", status: "Connected", kind: "direct", credential_present: true })).toBe("ready");
    expect(providerState({ state: "never_connected", status: "Offline", kind: "tunnel", credential_present: false })).toBe("never_connected");
  });
  it("falls back for a tunnel provider", () => {
    expect(providerState({ state: undefined as never, status: "Offline", kind: "tunnel", credential_present: false })).toBe("offline");
    expect(providerState({ state: undefined as never, status: "Connected", kind: "tunnel", credential_present: false })).toBe("online");
    expect(providerState({ state: undefined as never, status: "Degraded", kind: "tunnel", credential_present: false })).toBe("online");
  });
  it("falls back for a hosted provider without a credential", () => {
    expect(providerState({ state: undefined as never, status: "Offline", kind: "direct", credential_present: false })).toBe("not_configured");
  });
  it("falls back for a hosted provider with its credential", () => {
    expect(providerState({ state: undefined as never, status: "Connected", kind: "direct", credential_present: true })).toBe("ready");
  });
});

describe("the five states", () => {
  const STATES: State[] = ["online", "offline", "never_connected", "ready", "not_configured"];
  it("every state has a label and a badge kind", () => {
    expect(Object.keys(STATE_LABEL).sort()).toEqual([...STATES].sort());
    expect(Object.keys(STATE_BADGE).sort()).toEqual([...STATES].sort());
    for (const s of STATES) {
      expect(STATE_LABEL[s]).toMatch(/^[A-Z]/);
      expect(STATE_BADGE[s]).toMatch(/^status-(online|offline|never)$/);
    }
  });
  it("uses the words of the vocabulary", () => {
    expect(STATES.map((s) => STATE_LABEL[s])).toEqual(["Online", "Offline", "Never connected", "Ready", "Not configured"]);
  });
});
