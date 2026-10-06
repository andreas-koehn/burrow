# Connect a client

Install the `burrow` client, sign this machine in once, and put a local service online.

---

## Install, sign in, run

Three lines. The first two are needed once per machine.

**Linux and macOS**

```sh
curl -fsSL https://burrow.insingo.com/install.sh | sh
burrow login burrow.insingo.com
burrow http 3000
```

**Windows (PowerShell)**

```powershell
irm https://burrow.insingo.com/install.ps1 | iex
burrow login burrow.insingo.com
burrow http 3000
```

Use your own relay's address instead of `burrow.insingo.com`. The dashboard's
**Connect a client** page shows these lines with your relay filled in.

### 1. Install

The relay serves the installer, filled in with its own address and version.
It detects your operating system and CPU, downloads the matching archive
through the relay, checks its SHA-256 against the release's `checksums.txt`,
and installs one file: `~/.local/bin/burrow` on Linux and macOS (add
`--system` to install to `/usr/local/bin`, run with `sudo`), or
`%LOCALAPPDATA%\Programs\burrow\burrow.exe` on Windows. It does not sign in
and writes nothing else. It ends with the next command to run:

```
Installed burrow 0.7.0 (commit abc1234, built 2026-10-06, linux/amd64)
  at /home/you/.local/bin/burrow

Next: burrow login burrow.insingo.com
```

If the install directory is not on your `PATH`, the Linux and macOS installer
says so and prints the line to add to your shell profile. The Windows
installer adds its directory to your user `PATH`; open a new terminal
afterwards.

```sh
# system-wide install on Linux and macOS
curl -fsSL https://burrow.insingo.com/install.sh | sudo sh -s -- --system
```

::: warning What the checksum tells you
The check catches a download that is damaged, cut short or the wrong file. It
does not prove who made the file: `checksums.txt` is fetched from the same
place as the archive, so someone who controls the relay or the release host
controls both. Install from a relay you trust, and see
[Other ways to connect](#other-ways-to-connect) if you would rather download
and inspect the archive yourself.
:::

::: info Plain HTTP relays
The installers refuse a relay address that does not use HTTPS, unless it is on
this machine (`localhost`, a loopback address). Over plain HTTP the archive and
its checksums can be replaced on the way. To install from such a relay anyway,
set `BURROW_INSTALL_ALLOW_HTTP=1` for the shell that runs the script
(`$env:BURROW_INSTALL_ALLOW_HTTP = '1'` in PowerShell).
:::

### 2. Sign in

```sh
burrow login burrow.insingo.com
```

`burrow login` asks the relay where its control endpoint is, so you do not type
a port. It prints a page address and a short code, and opens the page in your
browser when it can:

```
Open this page to sign this machine in:

  https://burrow.insingo.com/link?code=BRRW-7Q4K

Check that the page shows the code BRRW-7Q4K.
Waiting for approval…  signed in as admin@insingo.com (token "kohns-laptop")
```

Log in to the dashboard if asked, check that the page shows the same code as
your terminal, and approve. The token is created at that moment and stored on
this machine; you never copy it. The request expires after ten minutes.

Approving a sign-in needs an admin account, or a role with the
`tokens:manage:own` or `tokens:manage:any` permission. The token belongs to the
person who approves. A user whose role has neither permission cannot approve a
sign-in, but can still create a token and use `burrow login --token -` (below).

The sign-in is stored in `config.yaml` in the `burrow` directory of your user
config directory, with mode `0600`. See [Configuration](/guide/configuration#user-config).

On a relay that does not offer browser sign-in, `burrow login` says so and
stops with the `--token -` command to use instead (exit code 2).

### 3. Run

```sh
burrow http 3000
```

The command stays in the foreground, reconnects by itself, and shows what is
happening:

```
burrow  ●  connected to burrow.insingo.com     v0.7.0   12 ms

  kohns-laptop-3000   https://burrow.insingo.com/svc/p7baeh/  →  127.0.0.1:3000
                      access: open (anyone with the URL)       3 open, 41 total

  14:02:11  GET   /api/users      200
  14:02:12  POST  /api/login      401
```

Press Ctrl-C to stop. The service gets the name `<hostname>-<port>` unless you
pass `--name`; running the same command again reuses the same service, URL and
access settings. Flags such as `--slug` and `--access`, and TCP services, are
covered in [Expose services](/guide/expose-services).

When stdout is not a terminal, or you pass `--log text` or `--log json`, the
client prints log lines instead of this view.

---

## Other ways to connect

### Download the archive yourself

Every relay hands out the archive for its own version at a fixed address:

```
https://burrow.insingo.com/download/burrow/<os>/<arch>
https://burrow.insingo.com/download/burrow/checksums.txt
```

`<os>` is `linux`, `darwin` or `windows`; `<arch>` is `amd64`, `arm64`, `arm` or
`386`. A version-tagged release builds eight pairs: `linux` on `amd64`, `arm64`,
`arm` and `386`; `darwin` on `amd64` and `arm64`; `windows` on `amd64` and
`386`. A relay built from an untagged commit hands out the rolling `develop`
build, which has `linux/amd64`, `linux/arm64`, `darwin/arm64` and
`windows/amd64`. The addresses redirect to the release's files, so use `-L`.

**Linux and macOS**

```sh
curl -fsSL -o burrow.tar.gz https://burrow.insingo.com/download/burrow/linux/amd64
curl -fsSL -o checksums.txt https://burrow.insingo.com/download/burrow/checksums.txt
grep "$(sha256sum burrow.tar.gz | cut -d' ' -f1)" checksums.txt
# macOS has no sha256sum:
grep "$(shasum -a 256 burrow.tar.gz | cut -d' ' -f1)" checksums.txt
```

The `grep` must print one line, naming the archive for your platform (for
example `burrow_linux_amd64_0.7.0.tar.gz`). No output means the download does
not match the release. Then unpack and install:

```sh
tar xzf burrow.tar.gz
sudo install -m 0755 burrow /usr/local/bin/burrow
```

**Windows**

```powershell
Invoke-WebRequest https://burrow.insingo.com/download/burrow/windows/amd64 -OutFile burrow.zip
Invoke-WebRequest https://burrow.insingo.com/download/burrow/checksums.txt -OutFile checksums.txt
(Get-FileHash burrow.zip -Algorithm SHA256).Hash
```

The hash must appear in `checksums.txt` on the line for your archive (compare
case-insensitively). Then extract `burrow.zip` and put `burrow.exe` in a
directory on your `PATH`.

The same archives are on the project's
[GitHub releases](https://github.com/andreas-koehn/burrow/releases) page. The
checksum has the same limit as in the installer: it catches damage, not a
replaced release.

::: info macOS and files downloaded in a browser
A file fetched with `curl` runs as it is. A file downloaded in a browser is
quarantined by macOS and will not start until you allow it in **System
Settings → Privacy & Security**. The binaries are not signed or notarised.
:::

To build from source:

```sh
git clone https://github.com/andreas-koehn/burrow.git
cd burrow
go build -o burrow ./cmd/client
# produces ./burrow (use -o burrow.exe on Windows)
```

### Sign in with a token (no browser)

On a machine without a browser, or against a relay without browser sign-in,
create a token in the dashboard and store it with `--token -`:

1. Log in to `https://burrow.insingo.com`.
2. Open **Clients**, then the **Tokens** tab. Enter a **Token name** (for
   example `laptop`), click **Create**, and copy the value.

```sh
burrow login burrow.insingo.com --token -
```

`burrow` asks for the token (input is hidden), or reads it from standard input
when you pipe it in. This keeps it out of your shell history and the process
list. Do not put the token itself on the command line.

::: tip Token starts with `bur_`
The token is shown once. Copy it now: it cannot be retrieved afterwards.
:::

You can also create a token on the host that runs the relay:

```sh
docker compose exec burrowd burrowd token \
  --email you@insingo.com \
  --name laptop
```

This writes directly to the database and prints the token.

If a relay is older and has no discovery endpoint, `burrow login` assumes the
control endpoint is `<relay host>:7000` and says so. Add `--control host:port`
only when your relay's control port is not 7000.

`burrow login` on a machine that is already signed in asks before replacing the
stored sign-in; `--force` skips the question. `burrow logout` forgets the
stored sign-in on this machine. It does not revoke the token: do that in the
dashboard under **Clients → Tokens**.

### `burrow.yaml` and `burrow up`

For several services, or settings you want in a file, write a `burrow.yaml`:

```yaml
services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http
    slug: my-app        # optional; used only when the service is created
    access: login       # optional: open, login or api-key; only when created
  - name: ssh
    local: 127.0.0.1:22
    type: tcp
    remote: 9001
```

```sh
burrow up
```

`burrow up` looks for the file in this order: `--file <path>`, `./burrow.yaml`,
then `burrow.yaml` in the user config directory. `server` and `token` (or
`token_file`) in the file are optional; what is missing comes from the sign-in.
`slug` and `access` are for `http` services and have the same create-only
meaning as the flags of `burrow http`.

| Key | Required | Notes |
|-----|----------|-------|
| `server` | No | Relay control address (`host:7000`); default from the sign-in |
| `token` | No | Bearer token; default from the sign-in |
| `token_file` | No | Path to a file containing the token (Docker Secrets / Kubernetes Secrets) |
| `services[].name` | Yes | Label shown in the dashboard |
| `services[].local` | Yes | Local address to forward traffic to (e.g. `127.0.0.1:3000`) |
| `services[].type` | No | `http` or `tcp`; defaults to `tcp` |
| `services[].remote` | No | Fixed public TCP port; TCP services only; `0` = auto-assigned |
| `services[].slug` | No | `http` only; path under `/svc/`; applied only when the service is created |
| `services[].access` | No | `http` only; `open`, `login` or `api-key`; applied only when the service is created |

A token from the sign-in is sent only to the relay it was stored for. If
`server` in the file or `BURROW_SERVER` names a different relay, the command
stops with exit code 3 and tells you to sign in to that relay, or give a token.

### Environment variables

`burrow http`, `burrow tcp`, `burrow up`, `burrow status` and the commands built
on them read these before the stored sign-in:

| Variable | Meaning |
|----------|---------|
| `BURROW_SERVER` | Control endpoint (`host:7000`) |
| `BURROW_TOKEN` | Token |
| `BURROW_TOKEN_FILE` | Path to a file holding the token |

See [Configuration](/guide/configuration#client-precedence) for the full order.
`burrow connect` does not read these variables.

### The explicit form: `burrow connect`

`burrow connect` is the form used before `login` existed. It is unchanged and
takes everything on the command line or in a `burrow.yaml` with `server` and a
token.

```sh
burrow connect --config burrow.yaml
```

```yaml
server: burrow.insingo.com:7000
token: bur_YOUR_TOKEN_HERE      # or token_file: /run/secrets/burrow-token

services:
  - name: my-app
    local: 127.0.0.1:3000
    type: http
```

For a quick one-off tunnel without a file:

```sh
burrow connect \
  --server burrow.insingo.com:7000 \
  --token bur_YOUR_TOKEN_HERE \
  --local 127.0.0.1:3000 \
  --type http \
  --name my-app
```

| Flag | Default | Notes |
|------|---------|-------|
| `--config` | | Path to `burrow.yaml`; needs `server` and a token in the file |
| `--server` | | Relay address (`host:port`) |
| `--token` | | Bearer token |
| `--local` | `127.0.0.1:3000` | Local address to tunnel |
| `--type` | `tcp` | `http` or `tcp` |
| `--name` | | Tunnel label |
| `--remote` | `0` | Requested TCP port (TCP type only) |
| `--insecure` | | Skip TLS verification (dev only) |
| `--cacert` | | Path to a custom CA PEM bundle |
| `--server-name` | | Override TLS server name |

`connect` ignores the stored sign-in and the `BURROW_SERVER` and `BURROW_TOKEN`
variables; a missing server or token is an error. It never shows the status
view.

::: tip TLS is validated automatically
The client connects to port `7000` using TLS. When the relay uses
Let's Encrypt (ACME), the certificate is signed by a public CA and validated by
the client automatically: no `--insecure` flag or extra CA bundle needed.
:::

::: warning `--insecure` in production
Only use `--insecure` against a local dev relay with self-signed certificates.
Never use it against a production relay.
:::

---

## Keep it running

`burrow service install` runs `burrow up` as a system service, for a machine
that should stay connected after you log out or restart. It needs a
`burrow.yaml` (the path you give, or the one in the user config directory) and a
stored sign-in. The service definition holds paths only; the token stays in the
sign-in's file.

```sh
burrow service install
burrow service status
burrow service logs
burrow service stop
burrow service start
burrow service uninstall
```

`install` refuses to install a second copy. `burrow service status` exits with
0 when the service is installed and 1 when it is not. `--cacert`,
`--server-name` and `--insecure` given to `install` are kept for the service.
Changes to `burrow.yaml` or a new `burrow login` reach the service only after
`burrow service uninstall` and `burrow service install`.

| Platform | What it is | Runs as | Elevated rights |
|----------|-----------|---------|-----------------|
| Linux | systemd system unit, restarted always | the user who ran `install` through `sudo` | yes: `sudo` |
| macOS | launchd agent in `~/Library/LaunchAgents` | the current user, while that user is logged in | no; `sudo` is refused |
| Windows | Windows service | LocalSystem | yes: elevated terminal |

**Linux.** Only systemd is supported. Run the install with `sudo` from the
account the service should run as; the unit runs as that user and reads that
user's own `0600` config, so nothing is copied. If `burrow` lives in
`~/.local/bin`, `sudo` may not find it; use the full path:

```sh
sudo "$(command -v burrow)" service install
```

Logs: `journalctl -u burrow` (`burrow service logs` prints the exact command).
When the install runs as real root, the binary, `burrow.yaml` and the sign-in
must belong to root and not be writable by others, all the way up the path.

**macOS.** The agent starts when you log in. Logs are in
`~/Library/Logs/burrow/burrow.err.log`.

**Windows.** The service runs as LocalSystem, so it refuses a binary that a
normal user can change. `burrow.exe` must be below `Program Files`. The
installer puts it in `%LOCALAPPDATA%\Programs\burrow`, so copy it first, from an
elevated terminal:

```powershell
New-Item -ItemType Directory -Force "$env:ProgramFiles\burrow"
Copy-Item "$env:LOCALAPPDATA\Programs\burrow\burrow.exe" "$env:ProgramFiles\burrow\"
& "$env:ProgramFiles\burrow\burrow.exe" service install
```

`burrow update` replaces the copy it was started from; it does not touch the
copy under `Program Files`, so repeat the copy after an update. The install
copies `burrow.yaml`, the sign-in and a custom CA file to `%ProgramData%\burrow`,
which only SYSTEM and Administrators can read. A `burrow.yaml` that names a
`token_file` is refused there; use the stored sign-in. The log is
`%ProgramData%\burrow\burrow.log`, rotated at 10 MB.

---

## Updating

```sh
burrow update
```

`burrow update` downloads the client that matches your relay's version through
the relay's download address, compares its SHA-256 with the release checksums
(the same limit as for the installer applies), runs it once with `version`, and
only then replaces the running binary. `--check` only reports whether an update
exists. Nothing updates by itself.

If the binary's directory is not writable, the command stops and prints what to
run instead (typically the same command with `sudo`). A relay running an older
version than your client is followed too, and the output calls that a
downgrade. A relay built from an untagged commit needs `--force`.

---

## Next steps

- [Expose services](/guide/expose-services) — HTTP vs TCP tunnels, names, slugs, access modes, apps behind a path.
- [Access control & security](/guide/access-control) — Lock a service behind an API key, Burrow login, or mTLS.
- [Troubleshooting](/guide/troubleshooting) — start with `burrow doctor`.
- [CLI reference](/reference/cli) — Full `burrow` command tree.
