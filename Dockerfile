# syntax=docker/dockerfile:1

# Build stage. CGO is off so the final image can be distroless/static: the
# metadata index uses a pure-Go SQLite driver precisely so that no libc is
# needed at runtime (see docs/adr/0005-eviction-policy.md).
FROM golang:1.23-bookworm AS build

WORKDIR /src

# Dependencies first, so that editing a .go file does not re-download modules.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X github.com/Lexieli666/kilncache/internal/buildinfo.Version=${VERSION} -X github.com/Lexieli666/kilncache/internal/buildinfo.Commit=${COMMIT}" \
      -o /out/kilncache ./cmd/kilncache

# An empty data directory, owned by the runtime user.
#
# This has to exist in the image, not just as a volume mount point. When Docker
# initialises a fresh named volume it copies the image's contents *and
# ownership* at that path; if the path does not exist in the image, the volume
# is created owned by root, and the nonroot user the container runs as cannot
# create anything in it. Distroless has no shell and no mkdir, so there is no
# way to fix it at startup -- the directory has to be right before the image is
# built. See docs/bugs.md.
RUN mkdir -p /out/data

# Runtime stage. Distroless static has no shell and no package manager, so a
# compromised cache node has nothing to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/kilncache /usr/local/bin/kilncache
COPY --from=build --chown=65532:65532 /out/data /var/lib/kilncache

ENV KILNCACHE_DATA_DIR=/var/lib/kilncache \
    KILNCACHE_LISTEN=:8080 \
    KILNCACHE_LOG_FORMAT=json

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/kilncache"]
