# Tool sources and `burrow mcp`

A **tool source** is an MCP server whose tools the relay's AI gateway serves to
apps and agents. Two kinds exist:

- a **remote** source is an MCP server on the internet that the relay calls
  (Streamable HTTP, https only);
- a **local** source is an MCP server on one of your machines. `burrow mcp`
  publishes it through a tunnel, and the relay never needs to reach that
  machine.

This page covers local sources. Callers reach every source's tools the same
way: at `/mcp/<source>` with the tools' own names, or at `/mcp` together with
the tools of every other source, named `<source>__<tool>`. They authenticate
with a gateway key whose tools allow-list names the source (for example
`files__*`).

---

## 1. Add the source in the dashboard {#dashboard}

Start in the dashboard. It creates what the relay needs, then shows you the
command to run.

1. Open **AI Gateway → Tools** and choose **Add local source**.
2. Enter a slug for the source: 1 to 24 characters, lowercase letters, digits
   and hyphens. `burrow` and `v1` are reserved. The slug is the prefix of the
   tool names on `/mcp`.
3. **Continue** creates two things: a service that only the AI gateway can
   reach, and the source on top of it. The dialog then shows the `burrow mcp`
   command and the matching `burrow.yaml` entry.
4. Run the command on the machine that has the MCP server (see
   [section 2](#cli)). The dialog waits until the client is connected and the
   first tool list has arrived.
5. Approve the tools. A new tool is **pending** and is not served until an
   admin approves it, either one by one or with **Approve all**. If a tool's
   definition changes later, it stops being served until it is approved
   again. The source's page shows the old and the new definition side by side.
6. Give a gateway key the tools: in the key's **Tools** section, pick the
   source (`files__*`) or name single tools (`files__read_file`).

`burrow mcp` changes nothing in the protocol between the client and the relay.
It registers an ordinary http tunnel named after the source, and that tunnel
binds to the service the dashboard created, by name. Without that service,
the relay creates a plain http service, which is not a tool source.

---

## 2. `burrow mcp` {#cli}

```
burrow mcp <name> -- <command> [args...]
burrow mcp <name> <http-url>
```

`<name>` is the name of the service the dashboard created. It is usually the
source's slug, and the dialog shows the exact command. The command signs in
like `burrow http` (with `burrow login`, or `BURROW_SERVER` and
`BURROW_TOKEN`). It runs in the foreground until it is interrupted and
reconnects by itself.

### A server that speaks stdio

Most MCP servers are programs that speak MCP over stdin and stdout. Put the
command after `--`. Everything after `--` belongs to that command, so burrow's
own flags go before it:

```sh
burrow mcp files -- npx -y @modelcontextprotocol/server-filesystem ~/docs

# The command gets burrow's environment: export a secret instead of
# putting it on the command line.
export GITHUB_PERSONAL_ACCESS_TOKEN=...
burrow mcp github -- \
  docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN ghcr.io/github/github-mcp-server
```

Do not pass secrets with `--env`. Everything on a command line can be read by
other users of the machine and ends up in shell history. Export the variable
before you run `burrow mcp`, or put it under `env` in `burrow.yaml`.

| Flag | Meaning |
|---|---|
| `--env KEY=VALUE` | Adds a variable to the command's environment. It can be repeated. The command also gets burrow's own environment. Use it for settings, not for secrets. |
| `--restart-max N` | Restarts the command at most N times, then gives up. `0` never restarts it. The default, `-1`, has no limit. |

### A server that already runs

A server that already listens for Streamable HTTP on your machine is given by
its URL:

```sh
burrow mcp notes http://localhost:3001/mcp
```

`--env` and `--restart-max` do not apply here: burrow does not start this
server. If it cannot be reached, burrow keeps trying, with a growing wait in
between.

### In `burrow.yaml`

`burrow up` runs local MCP servers next to the other services. Use
`type: mcp` with either `command` (plus optional `args` and `env`) or `url`:

```yaml
services:
  - name: files
    type: mcp
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/srv/docs"]
    env:
      LOG_LEVEL: info

  - name: notes
    type: mcp
    url: http://localhost:3001/mcp

  - name: web
    type: http
    local: 3000
```

An entry of type `mcp` must have exactly one of `command` and `url`. `args`
and `env` go with `command` only. `local` and `remote` do not apply, because
burrow picks the bridge address itself. `slug` and `access` work as they do
for `type: http`, but the dashboard flow has already set both. `burrow up`
restarts a command without limit, and `burrow connect --config` does not run
entries of type `mcp`.

### The status view

In a terminal, the service shows how many tools the server offers instead of
a local address:

```
  files   AI gateway only  →  MCP · 12 tools
          access: API key       0 open, 4 total
```

`MCP · starting` means that the first tool list has not arrived yet.
`· restarting` means that the command has ended and is being started again.
`· unreachable` means that the URL of a server that was already running does
not answer.
The status view replaces the log lines, so the server's stderr is not shown
there. Run with `--log text` to see it.

---

## 3. How it works {#how}

`burrow mcp` starts a small bridge on `127.0.0.1` at a random port. The
bridge is an MCP client to your server and also an MCP server toward the
relay. The tunnel's local address is the bridge, so the relay calls it like
any other http service. Because the bridge speaks MCP on both sides, your
server may use an older protocol revision than the relay. The bridge refuses
requests whose `Host` header is not a loopback address. This protects it
from DNS rebinding. The relay's tunnel sends the local address as the host,
so its requests pass.

Before the tunnel comes up, burrow waits up to 30 seconds for the server's
first tool list, so that the relay's first sync finds the tools. After that
it fetches the list again every 60 seconds, and at once when the server
announces a change.

---

## 4. Limits {#limits}

- **Rate limits count tool calls.** A tool call counts against the rate
  limits with a per-minute window that apply to it: those of the gateway
  key, those of the source's backing service, and global ones, as a model
  request does. Each source adds its own requests per minute and
  `max_concurrent` (0 = no limit). Day quotas and budgets neither count nor
  refuse tool calls.
- **A rename changes who may call a tool.** A key's allow-list names a tool
  by the name it is served under. When you rename a tool, keys that list the
  old name lose it and keys that already list the new name gain it; the
  dashboard is told which keys those are (`keys_matching_rename`).

- **Tools only.** The bridge passes `tools/list` and `tools/call` and nothing
  else. Resources, prompts, sampling, elicitation, roots and logging are not
  published. The relay refuses those methods, too.
- **Supervision of a command.**
  - The command is started once. Whenever it ends, it is started again after
    a growing wait: from half a second, doubling with random jitter, up to 30
    seconds. A run that lasted at least a minute starts the wait from the
    beginning again.
  - With `--restart-max N`, after N restarts the next exit ends `burrow mcp`
    with an error that says so.
  - Every line the command writes to stderr goes to burrow's log lines, with
    the source's name (`source=files`).
  - While the command is restarting, the bridge keeps serving the last tool
    list. A call in that time gets an error result that says the server is
    being started again. Keeping the list stops the tools from looking removed
    and then new to the relay, which would make them pending again.
  - The command runs in its own process group, so a Ctrl-C in the terminal
    goes to burrow and not straight to the command. On Windows it runs in its
    own job object instead.
  - When `burrow mcp` stops, the command and every process it started get
    SIGTERM. Whatever is still running 5 seconds later is killed. On Windows
    the job is killed at once.
  - If the command writes something to stdout that is not MCP, such as a
    banner line, the session ends. Burrow then ends the command and starts
    it again. Write logs to stderr instead.
- **Calls are not repeated.** If a call fails because the local server went
  away, it is not sent again, because the server may already have acted on
  it. The only exception is when a server that keeps sessions says that it no
  longer knows the session. That server has not run the call, so the bridge
  opens a new session and sends the call once more.
- **The bridge listens on loopback only**, but programs on the same machine
  can reach it without a key. The key check, allow-lists, limits, redaction
  and guardrails all happen at the relay.
