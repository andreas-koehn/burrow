import { describe, it, expect, beforeEach } from "vitest";
import { workspaceFor, rememberWorkspace, lastWorkspace, isShared } from "./workspace";

describe("workspaceFor", () => {
  beforeEach(() => localStorage.clear());
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
  it("a shared path takes the workspace used last", () => {
    expect(workspaceFor("/clients", "gateway")).toBe("gateway");
    expect(workspaceFor("/clients/connect", "gateway")).toBe("gateway");
    expect(workspaceFor("/clients", "services")).toBe("services");
  });
  it("other paths ignore it", () => {
    expect(workspaceFor("/services", "gateway")).toBe("services");
    expect(workspaceFor("/gateway/models", "services")).toBe("gateway");
    expect(workspaceFor("/settings/users", "gateway")).toBe("settings");
  });
  it("without an argument a shared path reads what is remembered", () => {
    expect(workspaceFor("/clients")).toBe("services");
    rememberWorkspace("/gateway/providers");
    expect(workspaceFor("/clients")).toBe("gateway");
    expect(workspaceFor("/clients/sess_1")).toBe("gateway");
  });
  // Layout passes "services" when the AI gateway is hidden, whatever is stored.
  it("a shared path is services for someone without the AI gateway", () => {
    rememberWorkspace("/gateway/providers");
    expect(workspaceFor("/clients", "services")).toBe("services");
  });
});

describe("isShared", () => {
  it("is /clients and everything under it", () => {
    for (const p of ["/clients", "/clients/", "/clients/connect", "/clients/sess_1"]) expect(isShared(p), p).toBe(true);
    for (const p of ["/", "/services", "/gateway", "/gateway/clients", "/clientsx", "/settings/users"]) expect(isShared(p), p).toBe(false);
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
  it("visiting a shared path does not change what is remembered", () => {
    rememberWorkspace("/gateway/providers");
    rememberWorkspace("/clients");
    expect(lastWorkspace()).toBe("gateway");
    rememberWorkspace("/services");
    rememberWorkspace("/clients/connect");
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
