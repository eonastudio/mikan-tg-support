# syntax=docker/dockerfile:1.7
# Build from the addon's root:
#   docker buildx build --platform linux/amd64 -f Dockerfile -t mikan-tg-support:dev --load .
#   docker buildx build --platform linux/amd64,linux/arm64 -f Dockerfile -t <registry>/mikan-tg-support:0.1.0 --push .
# Go cross-compiles on the build platform; only the final stage is the target's.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
COPY internal/ internal/
COPY web/ web/
# Filled in by BuildKit for each platform. A default here would win over it and put
# amd64 binaries into the arm64 image.
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X tg-support/internal/meta.Version=${VERSION}" -o /out/tg-support .
# The binary must be the image's own architecture: the ELF machine field says so.
RUN case "$TARGETARCH" in amd64) want="3e 00" ;; arm64) want="b7 00" ;; *) want="" ;; esac; \
    got=$(od -A n -t x1 -j 18 -N 2 /out/tg-support | tr -s " " | sed "s/^ //"); \
    if [ -n "$want" ] && [ "$got" != "$want" ]; then echo "the addon is not built for $TARGETARCH (ELF machine $got)"; exit 1; fi

# Static binary and CA certificates, no shell; runs as 65532 and writes no files (its
# state is in the panel's KV), so a read-only root file system is fine.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tg-support /tg-support
USER 65532:65532
ENTRYPOINT ["/tg-support"]
