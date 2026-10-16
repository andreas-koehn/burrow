/** Copy-ready configurations of the MCP clients. The token is always a variable, never a value. */
export function mcpSnippets(endpointURL: string): { claudeCode: string; cursor: string; vscode: string } {
  return {
    claudeCode: `claude mcp add --transport http burrow ${endpointURL} --header "Authorization: Bearer $BURROW_TOKEN"`,
    cursor: `{
  "mcpServers": {
    "burrow": {
      "url": "${endpointURL}",
      "headers": { "Authorization": "Bearer \${env:BURROW_TOKEN}" }
    }
  }
}`,
    vscode: `{
  "inputs": [{ "type": "promptString", "id": "burrow-token", "description": "Burrow automation token", "password": true }],
  "servers": {
    "burrow": {
      "type": "http",
      "url": "${endpointURL}",
      "headers": { "Authorization": "Bearer \${input:burrow-token}" }
    }
  }
}`,
  };
}
