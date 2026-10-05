import { describe, it, expect, beforeEach } from "vitest";
import { workspaceFor, rememberWorkspace, lastWorkspace } from "./workspace";

describe("workspaceFor", () => {
  it("derives the workspace from the URL alone", () => {
    const cases: Record<string, string> = {
      "/": "services", "/services": "services", "/services/abc": "services", "/clients": "services",
      "/traffic": "services", "/unknown": "services",
      "/gateway": "gateway", "/gateway/": "gateway", "/gateway/providers/zai": "gateway", "/gateway/cost": "gateway",
      "/settings": "settings", "/settings/users": "settings", "/settings/profile": "settings",
      // a path that merely starts with the same letters is not that workspace
      "/gateways": "services", "/settingsx": "services",
    };
    for (const [path, ws] of Object.entries(cases)) expect(workspaceFor(path), path).toBe(ws);
  });
});

describe("last workspace", () => {
  beforeEach(() => localStorage.clear());
  it("defaults to services", () => expect(lastWorkspace()).toBe("services"));
  it("remembers the last non-settings workspace", () => {
    rememberWorkspace("/gateway/providers");
    expect(lastWorkspace()).toBe("gateway");
    rememberWorkspace("/settings/users"); // settings is not a place to go back to
    expect(lastWorkspace()).toBe("gateway");
    rememberWorkspace("/services");
    expect(lastWorkspace()).toBe("services");
  });
  it("ignores a corrupted stored value", () => {
    localStorage.setItem("burrow.lastWorkspace", "nonsense");
    expect(lastWorkspace()).toBe("services");
  });
  it("survives storage that throws", () => {
    const orig = Storage.prototype.getItem;
    Storage.prototype.getItem = () => { throw new Error("blocked"); };
    try { expect(lastWorkspace()).toBe("services"); } finally { Storage.prototype.getItem = orig; }
  });
});
