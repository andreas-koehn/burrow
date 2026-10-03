# Multi-stage, multi-arch-friendly from-source build.
#
# Build with Docker Buildx (cross-compile so no QEMU emulation needed):
#   docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/ankoehn/burrow:develop .
#
# The web/dist directory is committed so no npm step is required here.

# ---------------------------------------------------------------------------
# Stage 1 – build both binaries for the requested target platform.
# $BUILDPLATFORM stays native so the compiler itself runs fast;
# $TARGETOS/$TARGETARCH are forwarded to GOOS/GOARCH for cross-compilation.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25 AS build

WORKDIR /src
COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w" \
    -o /out/burrowd ./cmd/server

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w" \
    -o /out/burrow ./cmd/client

# ---------------------------------------------------------------------------
# Stage 2 – minimal runtime image.
#
# Root-default distroless (NOT :nonroot) so burrowd can bind :80/:443 for
# built-in ACME without capability grants or a privileged flag.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/burrowd /usr/local/bin/burrowd
COPY --from=build /out/burrow  /usr/local/bin/burrow

EXPOSE 80 443 7000

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/burrowd", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/burrowd"]
CMD ["serve"]
