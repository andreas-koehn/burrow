import { describe, it, expect } from "vitest";
import { downloadTargets, installLines, isCommandName } from "./installSnippets";

describe("installLines", () => {
  it.each(["linux", "macos"] as const)("%s: curl installer, sign-in and run", (os) => {
    expect(installLines(os, "https://b.example.com")).toEqual({
      install: "curl -fsSL https://b.example.com/install.sh | sh",
      login: "burrow login b.example.com",
      run: "burrow http 3000",
    });
  });

  it("windows: the PowerShell installer, the other two the same", () => {
    expect(installLines("windows", "https://b.example.com")).toEqual({
      install: "irm https://b.example.com/install.ps1 | iex",
      login: "burrow login b.example.com",
      run: "burrow http 3000",
    });
  });

  it("keeps the port of the origin", () => {
    const l = installLines("linux", "https://b.example.com:8443");
    expect(l.install).toBe("curl -fsSL https://b.example.com:8443/install.sh | sh");
    expect(l.login).toBe("burrow login b.example.com:8443");
  });

  it("ignores a trailing slash on the origin", () => {
    expect(installLines("linux", "https://b.example.com/").install).toBe("curl -fsSL https://b.example.com/install.sh | sh");
  });

  it("a target replaces 3000, quoted when the shell needs it", () => {
    expect(installLines("linux", "https://b.example.com", "8080").run).toBe("burrow http 8080");
    expect(installLines("linux", "https://b.example.com", "my host:3000").run).toBe("burrow http 'my host:3000'");
  });

  it("with a name the run line serves the service of that name", () => {
    expect(installLines("linux", "https://b.example.com", "11434", "ollama").run).toBe("burrow http 11434 --name ollama");
    expect(installLines("windows", "https://b.example.com", "11434", "my_svc-2").run).toBe("burrow http 11434 --name my_svc-2");
    // The other two lines are the same with and without one.
    const { run: _a, ...withName } = installLines("linux", "https://b.example.com", "11434", "ollama");
    const { run: _b, ...without } = installLines("linux", "https://b.example.com", "11434");
    expect(withName).toEqual(without);
  });

  it.each(["x; rm -rf", "Ollama", "a b", "$(id)", "`id`", "a'b", "ä", "", "a\nb"])("a name that is not safe in a shell (%j) never reaches the command", (name) => {
    expect(isCommandName(name)).toBe(false);
    expect(installLines("linux", "https://b.example.com", "11434", name).run).toBe("burrow http 11434");
  });

  it("takes lower-case letters, digits, hyphen and underscore as a name", () => {
    expect(isCommandName("ollama-2_b")).toBe(true);
    expect(isCommandName(undefined)).toBe(false);
  });

  it("passes an origin that is not https through, scheme included", () => {
    const l = installLines("linux", "http://localhost:8080");
    expect(l.install).toBe("curl -fsSL http://localhost:8080/install.sh | sh");
    expect(l.login).toBe("burrow login http://localhost:8080");
  });

  it("never puts a token on a line: the sign-in goes through the browser", () => {
    for (const os of ["linux", "macos", "windows"] as const) {
      const l = installLines(os, "https://b.example.com");
      expect(Object.values(l).join("\n")).not.toMatch(/bur_/);
      expect(l.login).not.toMatch(/--token/);
    }
  });
});

describe("downloadTargets", () => {
  const hrefs = (v: string) => downloadTargets(v).flatMap((g) => g.builds.map((b) => b.href));

  it("a released relay offers every build of its release", () => {
    expect(hrefs("0.6.0")).toEqual([
      "/download/burrow/linux/amd64", "/download/burrow/linux/arm64", "/download/burrow/linux/arm", "/download/burrow/linux/386",
      "/download/burrow/darwin/amd64", "/download/burrow/darwin/arm64",
      "/download/burrow/windows/amd64", "/download/burrow/windows/386",
    ]);
    expect(hrefs("v1.2.3")).toHaveLength(8);
  });

  it.each(["develop", "dev", "0.6.0-12-gabc", "0.6.0-rc1", ""])("an untagged relay (%j) offers the four rolling builds", (v) => {
    expect(hrefs(v)).toEqual([
      "/download/burrow/linux/amd64", "/download/burrow/linux/arm64",
      "/download/burrow/darwin/arm64",
      "/download/burrow/windows/amd64",
    ]);
  });

  it("groups by operating system, in the order of the switch", () => {
    expect(downloadTargets("0.6.0").map((g) => [g.os, g.label])).toEqual([
      ["linux", "Linux"], ["macos", "macOS"], ["windows", "Windows"],
    ]);
  });
});
