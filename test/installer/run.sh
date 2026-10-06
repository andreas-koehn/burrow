#!/usr/bin/env bash
# Container matrix for the client installers (/install.sh, /install.ps1).
#
# Builds the client and the relay, packs the client the way a release does,
# starts real relays on a private Docker network — one per download
# directory: a good one, and one for each way a download can be wrong — and
# runs the served install script in Debian (dash, curl), Alpine (busybox sh,
# wget only) and, when the image can be pulled, PowerShell.
#
#   bash test/installer/run.sh
#
# Prints one ok/FAIL line per case and exits non-zero if any failed. Needs
# Docker and Go; GO names the go command (default: go on PATH, else the
# plan's container wrapper). Every container and the network are removed on
# exit; the two test images (burrow-installer-test:*) stay as a build cache.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"

if [ -z "${GO:-}" ]; then
    if command -v go >/dev/null 2>&1; then
        GO=go
    elif [ -x .claude/plans/client-cli/go.sh ]; then
        GO=.claude/plans/client-cli/go.sh
    else
        echo "run.sh: no go command; set GO" >&2
        exit 2
    fi
fi

# The work directory is inside the repository so a containerised go can
# write to it. It is given relative to the repository root.
WORK_REL=${BURROW_INSTALLER_WORK:-test/installer/.work}
WORK=$ROOT/$WORK_REL
RUN_ID="bit$$"
NET="burrow-installer-test-$RUN_ID"
LABEL="burrow-installer-test=$RUN_ID"
VERSION=9.9.9
PKG=github.com/ankoehn/burrow/internal/version

case "$(uname -m)" in
    x86_64 | amd64) ARCH=amd64 ;;
    aarch64 | arm64) ARCH=arm64 ;;
    *)
        echo "run.sh: unsupported host architecture $(uname -m)" >&2
        exit 2
        ;;
esac

PASS=0
FAILED=0

cleanup() {
    ids=$(docker ps -aq --filter "label=$LABEL" 2>/dev/null || true)
    if [ -n "$ids" ]; then
        # shellcheck disable=SC2086
        docker rm -f $ids >/dev/null 2>&1 || true
    fi
    docker network rm "$NET" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT

ok() {
    PASS=$((PASS + 1))
    printf 'ok    %s\n' "$1"
}

bad() {
    FAILED=$((FAILED + 1))
    printf 'FAIL  %s\n' "$1"
    shift
    printf '      %s\n' "$@"
}

# in_image <image> <user> <script>: runs the script with sh in a fresh
# container on the test network. Sets OUT (stdout+stderr) and RC. The relays
# here speak plain HTTP, which the installers refuse for anything but
# loopback, so BURROW_INSTALL_ALLOW_HTTP=1 is set; the cases about that
# refusal unset it again.
in_image() {
    set +e
    OUT=$(docker run --rm --label "$LABEL" --network "$NET" --user "$2" -e BURROW_INSTALL_ALLOW_HTTP=1 "$1" sh -c "$3" 2>&1 </dev/null)
    RC=$?
    set -e
}

# check <case name> <want exit: a number, or "nonzero"> <text that must appear>...
# A text starting with "!" must not appear.
check() {
    local name=$1 want=$2 problems=() text
    shift 2
    if [ "$want" = nonzero ]; then
        [ "$RC" -ne 0 ] || problems+=("exit $RC, want non-zero")
    else
        [ "$RC" -eq "$want" ] || problems+=("exit $RC, want $want")
    fi
    for text in "$@"; do
        if [ "${text#!}" != "$text" ]; then
            if printf '%s' "$OUT" | grep -qF -- "${text#!}"; then
                problems+=("output has: ${text#!}")
            fi
        elif ! printf '%s' "$OUT" | grep -qF -- "$text"; then
            problems+=("output lacks: $text")
        fi
    done
    if [ "${#problems[@]}" -eq 0 ]; then
        ok "$name"
    else
        bad "$name" "${problems[@]}" "--- output ---" "$(printf '%s' "$OUT" | tail -n 25)"
    fi
}

# ---------------------------------------------------------------- build ----
echo "== building burrow and burrowd ($ARCH) with: $GO"
rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/dl"
# netgo/osusergo keep the binaries static even where CGO_ENABLED cannot be
# passed to the go command (a containerised go), so they run on Alpine too.
build() { # <version> <output> <package>
    CGO_ENABLED=0 "$GO" build -trimpath -tags netgo,osusergo \
        -ldflags "-X $PKG.Version=$1" -o "$WORK_REL/bin/$2" "$3"
}
build "$VERSION" burrow ./cmd/client
build "$VERSION" burrowd ./cmd/server
build develop burrow-develop ./cmd/client
build develop burrowd-develop ./cmd/server

TGZ="burrow_linux_${ARCH}_${VERSION}.tar.gz"
ZIP="burrow_windows_amd64_${VERSION}.zip"
DEV_TGZ="burrow_linux_${ARCH}.tar.gz"

# pack <dir> <binary> <tar.gz name> [zip name]: a release-shaped directory.
pack() {
    local dir=$1 stage
    stage=$(mktemp -d "$WORK/stage.XXXXXX")
    mkdir -p "$dir"
    cp "$WORK/bin/$2" "$stage/burrow"
    chmod 755 "$stage/burrow"
    printf 'license\n' >"$stage/LICENSE"
    printf 'readme\n' >"$stage/README.md"
    # Like goreleaser: extra files next to the binary, binary not first.
    tar -czf "$dir/$3" -C "$stage" LICENSE README.md burrow
    if [ -n "${4:-}" ]; then
        # The PowerShell cases run on Linux: the "exe" is the Linux binary.
        cp "$stage/burrow" "$stage/burrow.exe"
        (cd "$stage" && zip -q "$dir/$4" LICENSE burrow.exe)
    fi
    (cd "$dir" && sha256sum -- * >checksums.txt)
    rm -rf "$stage"
}

DL=$WORK/dl
pack "$DL/good" burrow "$TGZ" "$ZIP"
pack "$DL/develop" burrow-develop "$DEV_TGZ"
# The same release behind a redirect: <base>/v<version>/<archive>.
pack "$DL/assets/dl/v$VERSION" burrow "$TGZ" "$ZIP"

variant() { # <name>: a copy of the good directory to break
    cp -r "$DL/good" "$DL/$1"
}
# One byte flipped after checksums.txt was written.
variant corrupt
python3 - "$DL/corrupt/$TGZ" "$DL/corrupt/$ZIP" <<'PY'
import sys
for path in sys.argv[1:]:
    data = bytearray(open(path, "rb").read())
    data[len(data) // 2] ^= 0x01
    open(path, "wb").write(data)
PY
# checksums.txt without a line for the archives.
variant nosum
grep -v -e "$TGZ" -e "$ZIP" "$DL/nosum/checksums.txt" >"$DL/nosum/c" || true
printf '%s  some_other_file.tar.gz\n' "$(printf x | sha256sum | cut -d' ' -f1)" >>"$DL/nosum/c"
mv "$DL/nosum/c" "$DL/nosum/checksums.txt"
# A line for the archive that is not a SHA-256.
variant badsum
printf 'not-a-checksum  %s\nnot-a-checksum  %s\n' "$TGZ" "$ZIP" >"$DL/badsum/checksums.txt"
# An HTML error page where the archive should be.
variant html
printf '<!doctype html><html><body><h1>502 Bad Gateway</h1></body></html>\n' >"$DL/html/$TGZ"
cp "$DL/html/$TGZ" "$DL/html/$ZIP"
# An HTML error page where checksums.txt should be.
variant htmlsum
printf '<!doctype html><html><body><h1>502 Bad Gateway</h1></body></html>\n' >"$DL/htmlsum/checksums.txt"
# An empty archive, and one cut off at half its length.
variant empty
: >"$DL/empty/$TGZ"
: >"$DL/empty/$ZIP"
variant truncated
head -c $(($(wc -c <"$DL/good/$TGZ") / 2)) "$DL/good/$TGZ" >"$DL/truncated/$TGZ"
# A correct checksum for an archive that holds no burrow binary.
mkdir -p "$DL/nobinary"
printf 'readme\n' >"$WORK/README.md"
tar -czf "$DL/nobinary/$TGZ" -C "$WORK" README.md
(cd "$DL/nobinary" && sha256sum -- * >checksums.txt)
# A correct checksum for an archive whose burrow does not run.
mkdir -p "$DL/broken" "$WORK/brokenstage"
printf 'this is not a program\n' >"$WORK/brokenstage/burrow"
tar -czf "$DL/broken/$TGZ" -C "$WORK/brokenstage" burrow
(cd "$DL/broken" && sha256sum -- * >checksums.txt)
# No archive at all: the relay answers 404.
mkdir -p "$DL/missing"
cp "$DL/good/checksums.txt" "$DL/missing/checksums.txt"
chmod -R a+rX "$WORK"

# -------------------------------------------------------------- network ----
echo "== starting relays"
docker network create --label "$LABEL" "$NET" >/dev/null
docker build -q -t burrow-installer-test:debian -f test/installer/Dockerfile.debian test/installer >/dev/null
docker build -q -t burrow-installer-test:alpine -f test/installer/Dockerfile.alpine test/installer >/dev/null
DEBIAN=burrow-installer-test:debian
ALPINE=burrow-installer-test:alpine

# relay <name> <binary> <env>...: a relay reachable as http://<name>:8080.
relay() {
    local name=$1 bin=$2 args=() e
    shift 2
    for e in "$@"; do args+=(-e "$e"); done
    docker run -d --label "$LABEL" --network "$NET" --name "$name-$RUN_ID" --network-alias "$name" --network-alias "127.$name.test" \
        -w /tmp -e BURROW_DATABASE_PATH=/tmp/burrow.db "${args[@]}" \
        -v "$WORK/bin/$bin:/burrowd:ro" -v "$DL:/dl:ro" \
        alpine:latest /burrowd serve --dev-certs >/dev/null
    RELAYS+=("$name")
}
RELAYS=()
for v in good corrupt nosum badsum html htmlsum empty truncated nobinary broken missing; do
    relay "relay-$v" burrowd "BURROW_CLIENT_DOWNLOAD_DIR=/dl/$v"
done
relay relay-develop burrowd-develop BURROW_CLIENT_DOWNLOAD_DIR=/dl/develop
relay relay-redirect burrowd BURROW_CLIENT_DOWNLOAD_BASE=http://assets/dl
docker run -d --label "$LABEL" --network "$NET" --name "assets-$RUN_ID" --network-alias assets \
    -v "$DL/assets:/usr/share/nginx/html:ro" nginx:alpine >/dev/null

for name in "${RELAYS[@]}"; do
    up=0
    for _ in $(seq 1 60); do
        if docker exec "$name-$RUN_ID" wget -q -O /dev/null http://127.0.0.1:8080/healthz 2>/dev/null; then
            up=1
            break
        fi
        sleep 0.5
    done
    if [ "$up" -ne 1 ]; then
        echo "run.sh: $name did not start" >&2
        docker logs "$name-$RUN_ID" 2>&1 | tail -n 20 >&2
        exit 2
    fi
done

GOOD=http://relay-good:8080

# ------------------------------------------------------------ shellcheck ----
docker exec "relay-good-$RUN_ID" wget -q -O - http://127.0.0.1:8080/install.sh >"$WORK/install.sh"
docker exec "relay-good-$RUN_ID" wget -q -O - http://127.0.0.1:8080/install.ps1 >"$WORK/install.ps1"
if docker pull -q koalaman/shellcheck-alpine:stable >/dev/null 2>&1; then
    set +e
    OUT=$(docker run --rm --label "$LABEL" -v "$WORK/install.sh:/install.sh:ro" koalaman/shellcheck-alpine:stable \
        shellcheck -s sh -S style /install.sh 2>&1)
    RC=$?
    set -e
    check "shellcheck -s sh on the served script: no findings" 0
else
    bad "shellcheck -s sh on the served script" "koalaman/shellcheck-alpine could not be pulled"
fi

# --------------------------------------------------------------- success ----
# FETCH is how each image gets a URL to stdout.
fetch_of() {
    if [ "$1" = "$DEBIAN" ]; then echo "curl -fsSL"; else echo "wget -qO-"; fi
}
label_of() {
    if [ "$1" = "$DEBIAN" ]; then echo "debian/dash/curl"; else echo "alpine/busybox/wget"; fi
}

in_image "$DEBIAN" root "curl -fsSL $GOOD/install.sh | sh && /root/.local/bin/burrow version && ls -A /root/.local/bin"
check "debian, root, curl | sh: installs to /root/.local/bin" 0 \
    "burrow $VERSION" "Next: burrow login relay-good:8080" "at /root/.local/bin/burrow" '!.burrow.new' '!rolling develop'

in_image "$DEBIAN" tester "curl -fsSL $GOOD/install.sh | sh && \$HOME/.local/bin/burrow version && ls -A \$HOME/.local/bin && ls -A /tmp | wc -l"
check "debian, non-root: installs to ~/.local/bin, prints the PATH hint, leaves no temp files" 0 \
    "burrow $VERSION" "/home/tester/.local/bin is not on your PATH" 'export PATH="/home/tester/.local/bin:$PATH"' '!.burrow.new'

in_image "$DEBIAN" tester "export PATH=\"\$HOME/.local/bin:\$PATH\"; curl -fsSL $GOOD/install.sh | sh && burrow version"
check "debian, non-root, directory already on PATH: no PATH hint" 0 "burrow $VERSION" '!is not on your PATH'

in_image "$DEBIAN" tester "curl -fsSL $GOOD/install.sh | sh -s -- --system; rc=\$?; [ ! -e /usr/local/bin/burrow ] && [ ! -e \$HOME/.local ] && echo NOTHING_INSTALLED; exit \$rc"
check "debian, non-root, --system: exit 1 with the sudo line, nothing installed" 1 \
    "curl -fsSL $GOOD/install.sh | sudo sh -s -- --system" "NOTHING_INSTALLED" '!Downloading'

in_image "$DEBIAN" root "curl -fsSL $GOOD/install.sh | sh -s -- --system && /usr/local/bin/burrow version && [ ! -e /root/.local ] && echo ONLY_SYSTEM"
check "debian, root, --system: installs to /usr/local/bin" 0 "burrow $VERSION" "ONLY_SYSTEM" '!is not on your PATH'

in_image "$ALPINE" tester "wget -qO- $GOOD/install.sh | sh && \$HOME/.local/bin/burrow version"
check "alpine (busybox sh, wget, no curl), non-root: installs" 0 "burrow $VERSION" "Next: burrow login relay-good:8080"

in_image "$ALPINE" tester "wget -qO- $GOOD/install.sh | sh -s -- --system; rc=\$?; [ ! -e /usr/local/bin/burrow ] && echo NOTHING_INSTALLED; exit \$rc"
check "alpine, non-root, --system: exit 1 with the wget sudo line" 1 \
    "wget -qO- $GOOD/install.sh | sudo sh -s -- --system" "NOTHING_INSTALLED"

in_image "$DEBIAN" tester "PATH=/tmp/nocurl:\$PATH; mkdir /tmp/nocurl; for t in sh wget tar gzip sha256sum uname mktemp cat chmod mv rm mkdir ls; do ln -s \"\$(command -v \$t)\" /tmp/nocurl/\$t; done; wget -qO- $GOOD/install.sh >/tmp/i.sh && PATH=/tmp/nocurl sh /tmp/i.sh && \$HOME/.local/bin/burrow version"
check "debian with GNU wget only (curl hidden): installs" 0 "burrow $VERSION"

# Plain HTTP without the variable: fine for loopback, where nothing travels.
# These two run inside the relay's own container.
for lo in 127.0.0.1 localhost; do
    set +e
    OUT=$(docker exec "relay-good-$RUN_ID" sh -c "rm -rf /root/.local; wget -qO- http://$lo:8080/install.sh | sh && /root/.local/bin/burrow version" 2>&1 </dev/null)
    RC=$?
    set -e
    check "plain http to $lo without BURROW_INSTALL_ALLOW_HTTP: installs" 0 "burrow $VERSION" "Next: burrow login $lo:8080"
done

# Addresses that look like this machine and are not, and the ones that are.
REFUSED_HOSTS="'localhost:80@evil.test' '127.0.0.1:80@evil.test' '[::1]:80@evil.test' '127.0.0.1:evil.test' '127.0.0.1.' '127.0.0.1.5' '127.999.1.1' '127.0.0.256' '127.1' '128.0.0.1' '0127.0.0.1' '127.0.0.0001' 'localhost.evil.test' 'localhost:' ':80' '' '[::1]x' '[::1]:' '[::2]' 'user@localhost' '127.0.0.1:80:80' '127..0.1' '.127.0.0.1' '127.0.0.01' '127.00.0.1' '127.0.0.010' '127.0.0.08'"
REFUSED_COUNT=27
LOOPBACK_HOSTS="'localhost' 'localhost:8080' '127.0.0.1' '127.0.0.1:9' '127.255.0.9:8080' '[::1]' '[::1]:8080'"
LOOPBACK_COUNT=7

for img in "$DEBIAN" "$ALPINE"; do
    f=$(fetch_of "$img")
    l=$(label_of "$img")

    in_image "$img" tester "$f http://relay-redirect:8080/install.sh | sh && \$HOME/.local/bin/burrow version"
    check "$l: download through the relay's 302 to another host" 0 "burrow $VERSION"

    in_image "$img" tester "$f http://relay-develop:8080/install.sh | sh && \$HOME/.local/bin/burrow version"
    check "$l: untagged relay installs the develop build and says so" 0 \
        "installing the rolling develop build" "Downloading $DEV_TGZ" "burrow develop"

    in_image "$img" tester "mkdir -p \$HOME/.local/bin && echo old >\$HOME/.local/bin/burrow; $f $GOOD/install.sh | sh && \$HOME/.local/bin/burrow version && ls -A \$HOME/.local/bin"
    check "$l: replaces an existing burrow" 0 "burrow $VERSION" '!.burrow.new'

    # The script is piped: nothing it starts may eat the rest of the pipe.
    in_image "$img" tester "$f $GOOD/install.sh >/tmp/i.sh; { cat /tmp/i.sh; echo 'echo TRAILER_RAN'; } | sh"
    check "$l: reads nothing from stdin" 0 "Next: burrow login" "TRAILER_RAN"

    # ----------------------------------------------------------- failures ----
    # Each failing run starts with an existing burrow, which must survive
    # byte for byte, with nothing else left in the directory or in TMPDIR.
    PRE="mkdir -p \$HOME/.local/bin /tmp/t && echo old >\$HOME/.local/bin/burrow && export TMPDIR=/tmp/t"
    POST="rc=\$?; [ \"\$(cat \$HOME/.local/bin/burrow)\" = old ] && [ \"\$(ls -A \$HOME/.local/bin)\" = burrow ] && echo OLD_UNTOUCHED; [ -z \"\$(ls -A /tmp/t)\" ] && echo TMP_CLEAN; exit \$rc"
    fails() { # <relay> <case name> <text>...
        local relay=$1 name=$2
        shift 2
        in_image "$img" tester "$PRE; $f http://$relay:8080/install.sh | sh; $POST"
        check "$l: $name" 1 "$@" "Nothing was installed" "OLD_UNTOUCHED" "TMP_CLEAN" '!Installed' '!Next: burrow login'
        in_image "$img" tester "$f http://$relay:8080/install.sh | sh; rc=\$?; [ ! -e \$HOME/.local ] && echo NO_FILE_CREATED; exit \$rc"
        check "$l: $name (fresh machine: no file created)" 1 "NO_FILE_CREATED"
    }
    fails relay-corrupt "one byte flipped in the archive" "checksum mismatch for $TGZ" "expected: " "got: "
    fails relay-nosum "checksums.txt without a line for the archive" "checksums.txt has no line for $TGZ"
    fails relay-badsum "checksums.txt line that is not a SHA-256" "does not hold a SHA-256"
    fails relay-html "an HTML error page instead of the archive" "checksum mismatch"
    fails relay-htmlsum "an HTML error page instead of checksums.txt" "checksums.txt has no line for $TGZ"
    fails relay-empty "an empty archive" "is empty"
    fails relay-truncated "an archive cut off at half its length" "checksum mismatch"
    fails relay-missing "no archive on the relay (404)" "could not download http://relay-missing:8080/download/burrow/linux/$ARCH"
    fails relay-nobinary "a verified archive without a burrow binary" "could not unpack burrow"
    fails relay-broken "a verified archive whose burrow does not run" "does not run on this machine"

    # A relay address without HTTPS is refused unless it is loopback or the
    # person says so with exactly BURROW_INSTALL_ALLOW_HTTP=1.
    in_image "$img" tester "unset BURROW_INSTALL_ALLOW_HTTP; $PRE; $f $GOOD/install.sh | sh; $POST"
    check "$l: plain http relay without BURROW_INSTALL_ALLOW_HTTP is refused" 1 "gave its address as $GOOD, without HTTPS" "BURROW_INSTALL_ALLOW_HTTP=1" "Nothing was installed" "OLD_UNTOUCHED" "TMP_CLEAN" '!Downloading'
    in_image "$img" tester "export BURROW_INSTALL_ALLOW_HTTP=yes; $PRE; $f $GOOD/install.sh | sh; $POST"
    check "$l: BURROW_INSTALL_ALLOW_HTTP=yes is not 1: refused" 1 "without HTTPS" "OLD_UNTOUCHED" '!Downloading'
    in_image "$img" tester "unset BURROW_INSTALL_ALLOW_HTTP; $PRE; $f http://127.relay-good.test:8080/install.sh | sh; $POST"
    check "$l: a host name that only starts with 127. is not loopback: refused" 1 "gave its address as http://127.relay-good.test:8080, without HTTPS" "OLD_UNTOUCHED" '!Downloading'

    # The script's own idea of "this machine", whatever address it was given:
    # RELAY is rewritten in the served script and the variable is not set.
    in_image "$img" tester "
        unset BURROW_INSTALL_ALLOW_HTTP
        $f $GOOD/install.sh >/tmp/i.sh
        r=0; a=0
        for h in $REFUSED_HOSTS; do
            sed \"s|^ *RELAY=.*|RELAY=\\\"http://\$h\\\"|\" /tmp/i.sh | sh >/tmp/out 2>&1
            if grep -q 'without HTTPS' /tmp/out && ! grep -q Downloading /tmp/out; then r=\$((r + 1)); else echo \"NOT_REFUSED <\$h>\"; fi
        done
        for h in $LOOPBACK_HOSTS; do
            sed \"s|^ *RELAY=.*|RELAY=\\\"http://\$h\\\"|\" /tmp/i.sh | sh >/tmp/out 2>&1
            if grep -q Downloading /tmp/out && ! grep -q 'without HTTPS' /tmp/out; then a=\$((a + 1)); else echo \"NOT_ACCEPTED <\$h>\"; fi
        done
        echo \"REFUSED_\$r ACCEPTED_\$a\"
        [ ! -e \$HOME/.local ] && echo NO_FILE_CREATED"
    check "$l: loopback is exactly localhost, [::1] or 127.x.y.z; look-alikes are refused" 0 "REFUSED_$REFUSED_COUNT ACCEPTED_$LOOPBACK_COUNT" "NO_FILE_CREATED" '!NOT_REFUSED' '!NOT_ACCEPTED'

    # Missing tools: named, before anything is downloaded.
    HIDE="mkdir /tmp/bin; for t in sh curl wget tar gzip sha256sum shasum uname mktemp cat chmod mv rm mkdir ls; do p=\$(command -v \$t) && ln -s \"\$p\" /tmp/bin/\$t; done; $f $GOOD/install.sh >/tmp/i.sh"
    in_image "$img" tester "$PRE; $HIDE; rm -f /tmp/bin/curl /tmp/bin/wget; PATH=/tmp/bin sh /tmp/i.sh; $POST"
    check "$l: neither curl nor wget" 1 "curl or wget is needed" "OLD_UNTOUCHED" "TMP_CLEAN" '!Downloading'
    in_image "$img" tester "$PRE; $HIDE; rm -f /tmp/bin/tar; PATH=/tmp/bin sh /tmp/i.sh; $POST"
    check "$l: no tar" 1 "tar is needed" "OLD_UNTOUCHED" "TMP_CLEAN" '!Downloading'
    in_image "$img" tester "$PRE; $HIDE; rm -f /tmp/bin/sha256sum /tmp/bin/shasum; PATH=/tmp/bin sh /tmp/i.sh; $POST"
    check "$l: neither sha256sum nor shasum" 1 "sha256sum or shasum is needed" "OLD_UNTOUCHED" "TMP_CLEAN" '!Downloading'

    # Unsupported platform: uname replaced by a wrapper first on PATH.
    FAKE="mkdir /tmp/fake; real=\$(command -v uname); printf '#!/bin/sh\ncase \"\$1\" in -m) echo \"\$FAKE_M\";; -s) echo \"\$FAKE_S\";; *) exec %s \"\$@\";; esac\n' \"\$real\" >/tmp/fake/uname; chmod +x /tmp/fake/uname; export PATH=/tmp/fake:\$PATH"
    in_image "$img" tester "$PRE; $FAKE; export FAKE_S=Linux FAKE_M=mips; $f $GOOD/install.sh | sh; $POST"
    check "$l: uname -m says mips" 1 "there is no burrow build for Linux mips" "Supported:" "  linux/$ARCH" "install.ps1" "OLD_UNTOUCHED" "TMP_CLEAN" '!Downloading'
    in_image "$img" tester "$PRE; $FAKE; export FAKE_S=FreeBSD FAKE_M=x86_64; $f $GOOD/install.sh | sh; $POST"
    check "$l: uname -s says FreeBSD" 1 "there is no burrow build for FreeBSD" "Supported:" "OLD_UNTOUCHED" '!Downloading'
    in_image "$img" tester "$PRE; $FAKE; export FAKE_S=Linux FAKE_M=armv7l; $f http://relay-develop:8080/install.sh | sh; $POST"
    check "$l: a platform only the tagged release builds, on a develop relay" 1 "there is no burrow build for linux/arm (develop channel)" "Supported:" "OLD_UNTOUCHED" '!Downloading'
    in_image "$img" tester "$PRE; $FAKE; export FAKE_S=Linux FAKE_M=i686; $f http://relay-redirect:8080/install.sh | sh; $POST"
    check "$l: a build the release lacks behind the redirect (404 from the asset host)" 1 "could not download http://relay-redirect:8080/download/burrow/linux/386" "OLD_UNTOUCHED" "TMP_CLEAN"

    # Install directory that cannot be written.
    in_image "$img" tester "mkdir -p \$HOME/.local/bin /tmp/t && chmod 555 \$HOME/.local/bin && export TMPDIR=/tmp/t; $f $GOOD/install.sh | sh; rc=\$?; [ -z \"\$(ls -A \$HOME/.local/bin)\" ] && echo DIR_EMPTY; [ -z \"\$(ls -A /tmp/t)\" ] && echo TMP_CLEAN; exit \$rc"
    check "$l: ~/.local/bin not writable" 1 "is not a writable directory" "Nothing was installed" "DIR_EMPTY" "TMP_CLEAN" '!Downloading'
    in_image "$img" tester "mkdir /tmp/t && export TMPDIR=/tmp/t; chmod 555 \$HOME; $f $GOOD/install.sh | sh; rc=\$?; [ ! -e \$HOME/.local ] && echo NO_FILE_CREATED; [ -z \"\$(ls -A /tmp/t)\" ] && echo TMP_CLEAN; exit \$rc"
    check "$l: home directory not writable" 1 "cannot create /home/tester/.local/bin" "NO_FILE_CREATED" "TMP_CLEAN"
    in_image "$img" tester "export HOME=relative/path; $f $GOOD/install.sh | sh"
    check "$l: HOME is not an absolute path" 1 "HOME is not an absolute path" '!Downloading'
    in_image "$img" tester "mkdir -p \$HOME/.local/bin/burrow; $f $GOOD/install.sh | sh"
    check "$l: the target is a directory" 1 "is a directory" '!Downloading'
    in_image "$img" tester "$f $GOOD/install.sh | sh -s -- --prefix /opt"
    check "$l: unknown option" 1 "unknown option: --prefix" '!Downloading'

    # Interrupted: a wrapper for one tool signals the installer while it runs.
    # tar runs after the download is verified, chmod after the staged file
    # exists next to the target.
    for tool in tar chmod; do
        for sig in TERM INT HUP; do
            in_image "$img" tester "$PRE; mkdir /tmp/w; printf '#!/bin/sh\nkill -$sig \$PPID\nsleep 1\n' >/tmp/w/$tool; chmod +x /tmp/w/$tool; $f $GOOD/install.sh >/tmp/i.sh; PATH=/tmp/w:\$PATH sh /tmp/i.sh; $POST"
            check "$l: SIG$sig during $tool" nonzero "OLD_UNTOUCHED" "TMP_CLEAN" '!Installed'
        done
    done

    # Cut off: a prefix of the script either does nothing or does exactly
    # what the whole script does with the same arguments. It is run with
    # --system as a user who may not write /usr/local/bin: the whole script
    # refuses with the sudo line; a prefix that ran main without its
    # arguments would install to ~/.local/bin instead. Cuts: nine across the
    # script and every byte offset of the last 40 bytes.
    in_image "$img" tester "
        $f $GOOD/install.sh >/tmp/i.sh
        total=\$(wc -c </tmp/i.sh)
        n=0
        kept=0
        for cut in \$((total / 2)) \$((total / 16)) \$((total / 8)) \$((total / 4)) \$((total * 3 / 8)) \$((total * 5 / 8)) \$((total * 3 / 4)) \$((total * 7 / 8)) \$((total * 15 / 16)) \$(seq \$((total - 40)) \$((total - 1))); do
            head -c \"\$cut\" /tmp/i.sh | sh -s -- --system >/tmp/out 2>&1; rc=\$?
            if [ -e \$HOME/.local ] || [ -e /usr/local/bin/burrow ] || grep -q -e Downloading -e Installed /tmp/out; then
                echo \"CUT_AT_\${cut}_RAN (exit \$rc)\"; cat /tmp/out
            fi
            if grep -q 'is not writable by this user' /tmp/out; then kept=\$((kept + 1)); fi
            n=\$((n + 1))
        done
        echo \"TRIED_\$n\"
        echo \"KEPT_ARGUMENTS_\$kept\"
        head -c \$((total / 2)) /tmp/i.sh | sh >/dev/null 2>&1; echo \"HALF_EXIT_\$?\"
        [ ! -e \$HOME/.local ] && echo NO_FILE_CREATED"
    # Only the cut that drops the final newline leaves a whole script.
    check "$l: the script cut off at half its length, 8 other points and each of its last 40 bytes" 0 "TRIED_49" "KEPT_ARGUMENTS_1" "NO_FILE_CREATED" '!_RAN' '!HALF_EXIT_0'

    # Every strict prefix of the script, byte by byte, with a PATH that holds
    # nothing but stubs which record being called: no prefix may run a
    # command (the stubs stay silent), run a piece of a word as a command
    # (no "not found" from the shell) or leave a file in the home directory.
    # The last cut only drops the final newline; it is the whole script and
    # shows that the stubs do notice a script that runs.
    in_image "$img" tester "
        $f $GOOD/install.sh >/tmp/i.sh
        mkdir /tmp/stubs /tmp/dest
        for c in curl wget tar gzip sha256sum shasum uname mktemp cat chmod mv rm rmdir mkdir cp ln sed awk grep head tail sudo su id env printf; do
            printf '#!/bin/sh\\necho \"\$0\" >>/tmp/calls\\n' >/tmp/stubs/\$c; chmod +x /tmp/stubs/\$c
        done
        total=\$(wc -c </tmp/i.sh)
        : >/tmp/calls; : >/tmp/err
        cut=0
        while [ \$cut -le \$((total - 2)) ]; do
            head -c \$cut /tmp/i.sh | HOME=/tmp/dest PATH=/tmp/stubs /bin/sh >>/tmp/err 2>&1
            cut=\$((cut + 1))
        done
        echo \"SWEPT_\$cut OF_\$((total - 1))\"
        [ ! -s /tmp/calls ] && echo NO_COMMAND_RAN
        grep -q 'not found' /tmp/err || echo NO_FRAGMENT_RAN
        [ -z \"\$(ls -A /tmp/dest)\" ] && echo DEST_EMPTY
        grep -c -i 'syntax error' /tmp/err | sed 's/^/SYNTAX_ERRORS_/'
        head -c \$((total - 1)) /tmp/i.sh | HOME=/tmp/dest PATH=/tmp/stubs /bin/sh >/dev/null 2>&1
        [ -s /tmp/calls ] && echo DETECTOR_SEES_THE_WHOLE_SCRIPT"
    check "$l: every strict prefix of the script, byte by byte: no command runs, nothing is written" 0 \
        "SWEPT_" "NO_COMMAND_RAN" "NO_FRAGMENT_RAN" "DEST_EMPTY" "DETECTOR_SEES_THE_WHOLE_SCRIPT"

    # The same as root, where the whole script installs to /usr/local/bin:
    # no prefix may install to /root/.local/bin instead.
    in_image "$img" root "
        $f $GOOD/install.sh >/tmp/i.sh
        total=\$(wc -c </tmp/i.sh)
        system=0
        for cut in \$(seq \$((total - 40)) \$((total - 1))); do
            head -c \"\$cut\" /tmp/i.sh | sh -s -- --system >/tmp/out 2>&1
            if [ -e /root/.local ]; then echo \"CUT_AT_\${cut}_DROPPED_THE_ARGUMENTS\"; rm -rf /root/.local; fi
            if [ -e /usr/local/bin/burrow ]; then system=\$((system + 1)); rm -f /usr/local/bin/burrow; fi
        done
        echo \"SYSTEM_INSTALLS_\$system\""
    check "$l: as root with --system, no cut in the last 40 bytes installs to the home directory" 0 "SYSTEM_INSTALLS_1" '!DROPPED_THE_ARGUMENTS'
done

# ------------------------------------------------------------ PowerShell ----
PWSH=mcr.microsoft.com/powershell:latest
if docker pull -q "$PWSH" >/dev/null 2>&1; then
    # Linux PowerShell: LOCALAPPDATA and PROCESSOR_ARCHITECTURE are given, the
    # user Path cannot be stored. Everything else is the script as served.
    ps_case() { # <relay> <arch> <more PowerShell>
        set +e
        OUT=$(docker run --rm --label "$LABEL" --network "$NET" -e LOCALAPPDATA=/tmp/lad -e "PROCESSOR_ARCHITECTURE=$2" -e BURROW_INSTALL_ALLOW_HTTP=1 "$PWSH" \
            pwsh -NoProfile -NonInteractive -Command "
                \$ErrorActionPreference = 'Stop'
                New-Item -ItemType Directory -Path /tmp/lad/Programs/burrow -Force | Out-Null
                Set-Content -Path /tmp/lad/Programs/burrow/burrow.exe -Value old -NoNewline
                \$before = @(Get-ChildItem ([IO.Path]::GetTempPath())).Count
                \$failed = \$false
                try { $3 } catch { \$failed = \$true; Write-Host \$_.Exception.Message }
                \$exe = Get-Content -Raw /tmp/lad/Programs/burrow/burrow.exe
                if (\$exe -eq 'old') { Write-Host OLD_UNTOUCHED } else { Write-Host REPLACED }
                if (@(Get-ChildItem /tmp/lad/Programs/burrow).Count -eq 1) { Write-Host DIR_CLEAN }
                if (@(Get-ChildItem ([IO.Path]::GetTempPath())).Count -eq \$before) { Write-Host TMP_CLEAN }
                if (\$failed) { exit 1 }
            " 2>&1 </dev/null)
        RC=$?
        set -e
    }
    if [ "$ARCH" = amd64 ]; then
        ps_case relay-good AMD64 "Invoke-Expression (Invoke-RestMethod http://relay-good:8080/install.ps1)"
        check "powershell: installs to LOCALAPPDATA/Programs/burrow" 0 \
            "Downloading $ZIP" "Installed burrow" "Next: burrow login relay-good:8080" "REPLACED" "DIR_CLEAN" "TMP_CLEAN"
        ps_case relay-redirect AMD64 "Invoke-Expression (Invoke-RestMethod http://relay-redirect:8080/install.ps1)"
        check "powershell: download through the relay's 302" 0 "Installed burrow" "REPLACED" "TMP_CLEAN"
    fi
    ps_fails() { # <relay> <case name> <text>...
        local relay=$1 name=$2
        shift 2
        ps_case "$relay" AMD64 "Invoke-Expression (Invoke-RestMethod http://$relay:8080/install.ps1)"
        check "powershell: $name" 1 "$@" "Nothing was installed" "OLD_UNTOUCHED" "DIR_CLEAN" "TMP_CLEAN" '!Installed burrow' '!Next: burrow login'
    }
    ps_fails relay-corrupt "one byte flipped in the archive" "checksum mismatch for $ZIP" "expected: " "got: "
    ps_fails relay-nosum "checksums.txt without a line for the archive" "checksums.txt has no line for $ZIP"
    ps_fails relay-badsum "checksums.txt line that is not a SHA-256" "does not hold a SHA-256"
    ps_fails relay-html "an HTML error page instead of the archive" "checksum mismatch"
    ps_fails relay-htmlsum "an HTML error page instead of checksums.txt" "checksums.txt has no line for $ZIP"
    ps_fails relay-empty "an empty archive" "is empty"
    ps_fails relay-missing "no archive on the relay (404)" "could not download http://relay-missing:8080/download/burrow/windows/amd64"
    ps_case relay-good AMD64 "\$env:BURROW_INSTALL_ALLOW_HTTP = \$null; Invoke-Expression (Invoke-RestMethod http://relay-good:8080/install.ps1)"
    check "powershell: plain http relay without BURROW_INSTALL_ALLOW_HTTP is refused" 1 "gave its address as http://relay-good:8080, without HTTPS" "Nothing was installed" "OLD_UNTOUCHED" "DIR_CLEAN" "TMP_CLEAN" '!Downloading'
    ps_case relay-good AMD64 "\$env:BURROW_INSTALL_ALLOW_HTTP = \$null; Invoke-Expression (Invoke-RestMethod http://127.relay-good.test:8080/install.ps1)"
    check "powershell: a host name that only starts with 127. is not loopback: refused" 1 "without HTTPS" "OLD_UNTOUCHED" '!Downloading'
    for a in ARM64 x86 ""; do
        ps_case relay-good "$a" "Invoke-Expression (Invoke-RestMethod http://relay-good:8080/install.ps1)"
        check "powershell: PROCESSOR_ARCHITECTURE '$a' is refused" 1 "there is no burrow build this installer can set up" "Supported: windows/" "OLD_UNTOUCHED" "TMP_CLEAN" '!Downloading'
    done
    ps_case relay-good AMD64 "\$s = Invoke-RestMethod http://relay-good:8080/install.ps1; Invoke-Expression \$s.Substring(0, [int](\$s.Length / 2))"
    check "powershell: the script cut off at half its length" 1 "OLD_UNTOUCHED" "DIR_CLEAN" '!Downloading'
    ps_case relay-good AMD64 "\$s = Invoke-RestMethod http://relay-good:8080/install.ps1; Invoke-Expression \$s.Substring(0, \$s.LastIndexOf('Install-Burrow'))"
    check "powershell: the script without its last two lines does not parse" 1 "OLD_UNTOUCHED" "DIR_CLEAN" '!Downloading'
    # Every strict prefix, character by character: comments only, or a parse
    # error. Nothing may run and nothing may fail in another way.
    ps_case relay-good AMD64 "
        \$s = (Invoke-RestMethod http://relay-good:8080/install.ps1).TrimEnd()
        # The block opens on a line of its own; the comments above mention it.
        \$start = \$s.IndexOf(\"\`n& {\") + 1
        \$ran = 0; \$parse = 0; \$comment = 0
        for (\$i = 1; \$i -lt \$s.Length; \$i++) {
            try {
                Invoke-Expression \$s.Substring(0, \$i)
                if (\$i -gt \$start) { \$ran++; Write-Host \"RAN_AT_\$i\" } else { \$comment++ }
            } catch [System.Management.Automation.ParseException] {
                \$parse++
            } catch {
                \$ran++; Write-Host \"OTHER_ERROR_AT_\$i \$(\$_.Exception.Message)\"
            }
        }
        Write-Host \"PREFIXES_RAN_\$ran COMMENT_ONLY_IS_\$(\$comment -eq \$start) PARSE_ERRORS_ARE_THE_REST_\$(\$parse -eq \$s.Length - 1 - \$start)\""
    check "powershell: every strict prefix of the script, character by character, is comments or a parse error" 0 \
        "PREFIXES_RAN_0 COMMENT_ONLY_IS_True PARSE_ERRORS_ARE_THE_REST_True" "OLD_UNTOUCHED" "DIR_CLEAN" '!Downloading'
    # Two more hosts for PowerShell alone: a line break after the address. A
    # dollar sign in a pattern matches before it; the script's patterns end
    # with \z.
    ps_case relay-good AMD64 "
        \$env:BURROW_INSTALL_ALLOW_HTTP = \$null
        \$s = Invoke-RestMethod http://relay-good:8080/install.ps1
        \$r = 0; \$a = 0
        foreach (\$h in @($(printf '%s' "$REFUSED_HOSTS" | sed "s/' '/', '/g")) + @(\"127.0.0.1\`n\", \"127.0.0.1:80\`n\")) {
            try { Invoke-Expression \$s.Replace(\"'http://relay-good:8080'\", \"'http://\$h'\"); Write-Host \"NOT_REFUSED <\$h>\" }
            catch { if (\$_.Exception.Message -match 'without HTTPS') { \$r++ } else { Write-Host \"NOT_REFUSED <\$h> \$(\$_.Exception.Message)\" } }
        }
        foreach (\$h in @($(printf '%s' "$LOOPBACK_HOSTS" | sed "s/' '/', '/g"))) {
            try { Invoke-Expression \$s.Replace(\"'http://relay-good:8080'\", \"'http://\$h'\"); Write-Host \"UNEXPECTED_INSTALL <\$h>\" }
            catch { if (\$_.Exception.Message -match 'could not download') { \$a++ } else { Write-Host \"NOT_ACCEPTED <\$h> \$(\$_.Exception.Message)\" } }
        }
        Write-Host \"REFUSED_\$r ACCEPTED_\$a\""
    check "powershell: loopback is exactly localhost, [::1] or 127.x.y.z; look-alikes are refused" 0 \
        "REFUSED_$((REFUSED_COUNT + 2)) ACCEPTED_$LOOPBACK_COUNT" "OLD_UNTOUCHED" '!NOT_REFUSED' '!NOT_ACCEPTED' '!UNEXPECTED_INSTALL'
else
    bad "powershell cases" "$PWSH could not be pulled; install.ps1 was not run"
fi

echo
echo "$PASS ok, $FAILED failed"
[ "$FAILED" -eq 0 ]
