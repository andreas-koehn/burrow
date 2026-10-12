import { describe, it, expect } from "vitest";
import { CLIENT_OS_OPTIONS, downloadTargets, installLines, isCommandName, isTarget, targetOf } from "./installSnippets";

// Every operating system the lines are written for.
const EVERY_OS = CLIENT_OS_OPTIONS.map((o) => o.value);
// What a command may consist of when everything in it was let through on purpose.
const PLAIN_RUN = /^burrow http (?:[A-Za-z0-9.:-]+|'[A-Za-z0-9.:[\]-]+')(?: --name [a-z0-9][a-z0-9_-]*)?$/;

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

  it("a target replaces 3000: a port, or host:port", () => {
    expect(installLines("linux", "https://b.example.com", "8080").run).toBe("burrow http 8080");
    expect(installLines("linux", "https://b.example.com", "192.168.1.20:8000").run).toBe("burrow http 192.168.1.20:8000");
    expect(installLines("windows", "https://b.example.com", "gpu-box.lan:11434").run).toBe("burrow http gpu-box.lan:11434");
  });

  // The brackets would be a pattern to a POSIX shell; single quotes are
  // literal there and in PowerShell alike, and a target holds no quote.
  it.each(EVERY_OS)("%s: an IPv6 address is quoted", (os) => {
    expect(installLines(os, "https://b.example.com", "[::1]:8000").run).toBe("burrow http '[::1]:8000'");
  });

  it.each(["8080", "1", "65535", "localhost:8000", "127.0.0.1:11434", "gpu-box.lan:1234", "[::1]:8000", "[fe80::1]:80"])(
    "takes %j as a target", (target) => {
      expect(isTarget(target)).toBe(true);
    });

  // The address a service was last served from is what its client reported:
  // the relay does not check it, and the line is pasted into a shell.
  describe.each(EVERY_OS)("%s: a target that is not a port or host:port", (os) => {
    it.each([
      "x';calc;'y:80", "x;calc:80", "x`calc`:80", "$(calc):80", "x|calc:80", "x&calc:80", "a b:80", "x\"y:80", "x\ny:80",
      "http://x:80", "x:80/path", "-x:80", "x-:80", "[::1;calc]:80", "[::1]", "x:", ":80", "x:0", "x:65536", "x:123456", "0", "", "host",
    ])("%j never reaches the command", (target) => {
      expect(isTarget(target)).toBe(false);
      const { run } = installLines(os, "https://b.example.com", target, "ollama");
      expect(run).toBe("burrow http 3000 --name ollama");
      expect(run).toMatch(PLAIN_RUN);
    });
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

  // A name that begins with a hyphen would be read as a flag of its own.
  it.each(["--config", "-x", "-", "--name", "_x"])("a name that does not begin with a letter or digit (%j) never reaches the command", (name) => {
    expect(isCommandName(name)).toBe(false);
    for (const os of EVERY_OS) {
      expect(installLines(os, "https://b.example.com", "11434", name).run).toBe("burrow http 11434");
    }
  });

  it("takes lower-case letters, digits, hyphen and underscore as a name", () => {
    expect(isCommandName("ollama-2_b")).toBe(true);
    expect(isCommandName("2nd-box")).toBe(true);
    expect(isCommandName("a")).toBe(true);
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

describe("targetOf", () => {
  it.each([
    ["127.0.0.1:11434", "11434"],
    ["localhost:8000", "8000"],
    ["[::1]:8000", "8000"],
    [":8000", "8000"],
    // Another host is kept whole: the port alone would name this machine.
    ["192.168.1.5:8000", "192.168.1.5:8000"],
  ])("%s gives %s", (addr, target) => {
    expect(targetOf(addr)).toBe(target);
  });

  it.each(["", "ollama", "11434"])("%j gives nothing: no port is known", (addr) => {
    expect(targetOf(addr)).toBeUndefined();
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
