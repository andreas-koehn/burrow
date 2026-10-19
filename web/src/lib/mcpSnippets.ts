/** How a snippet names the server and holds its credential. */
export interface McpClientNames {
  /** The server's name in the client. */
  server: string;
  /** The environment variable (Claude Code, Cursor) that holds the credential. */
  env: string;
  /** The VS Code input id and its description. */
  input: string;
  inputDescription: string;
}

/** The management server: an automation token. */
export const MANAGEMENT_CLIENT: McpClientNames = {
  server: "burrow", env: "BURROW_TOKEN", input: "burrow-token", inputDescription: "Burrow automation token",
};

/** The tool gateway at /mcp: a gateway key. */
export const GATEWAY_KEY_CLIENT: McpClientNames = {
  server: "burrow-tools", env: "BURROW_GATEWAY_KEY", input: "burrow-gateway-key", inputDescription: "Burrow gateway key",
};

/** Copy-ready configurations of the MCP clients. The token is always a variable, never a value. */
export function mcpSnippets(
  endpointURL: string,
  n: McpClientNames = MANAGEMENT_CLIENT,
): { claudeCode: string; cursor: string; vscode: string } {
  return {
    claudeCode: `claude mcp add --transport http ${n.server} ${endpointURL} --header "Authorization: Bearer $${n.env}"`,
    cursor: `{
  "mcpServers": {
    "${n.server}": {
      "url": "${endpointURL}",
      "headers": { "Authorization": "Bearer \${env:${n.env}}" }
    }
  }
}`,
    vscode: `{
  "inputs": [{ "type": "promptString", "id": "${n.input}", "description": "${n.inputDescription}", "password": true }],
  "servers": {
    "${n.server}": {
      "type": "http",
      "url": "${endpointURL}",
      "headers": { "Authorization": "Bearer \${input:${n.input}}" }
    }
  }
}`,
  };
}

/** For clients that sign in with OAuth: the address alone, no credential in the file. */
export function mcpOAuthSnippets(
  endpointURL: string,
  n: McpClientNames = GATEWAY_KEY_CLIENT,
): { claudeCode: string; cursor: string; vscode: string } {
  return {
    claudeCode: `claude mcp add --transport http ${n.server} ${endpointURL}`,
    cursor: `{
  "mcpServers": {
    "${n.server}": { "url": "${endpointURL}" }
  }
}`,
    vscode: `{
  "servers": {
    "${n.server}": { "type": "http", "url": "${endpointURL}" }
  }
}`,
  };
}
