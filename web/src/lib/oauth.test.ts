import { describe, it, expect } from "vitest";
import { validRedirectUri } from "./oauth";

// The same table as TestRedirectURIRules in internal/oauth/oauth_test.go.
describe("validRedirectUri", () => {
  it.each([
    ["https://app.example/cb", true],
    ["http://127.0.0.1:8080/cb", true],
    ["http://localhost/cb", true],
    ["http://[::1]:9/cb", true],
    ["com.example.app:/oauth", true],
    ["HTTPS://App.Example/cb", true],
    ["http://127.0.0.2/cb", true],
    ["http://app.example/cb", false],
    ["https://app.example/cb#frag", false],
    ["https://user:pw@app.example/cb", false],
    ["javascript:alert(1)", false],
    ["cursor://x", false],
    ["", false],
    ["https:///nohost", false],
    ["https://app.example/a b", false],
    ["data:text/html,<script>", false],
    [`https://${"a".repeat(3000)}`, false],
    ["http://127.1/cb", false],
    ["https://app.example:port/cb", false],
    ["ftp://x", false],
  ])("%s → %s", (raw, ok) => {
    expect(validRedirectUri(raw)).toBe(ok);
  });
});
