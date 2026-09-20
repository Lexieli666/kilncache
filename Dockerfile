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

# Runtime stage. Distroless static has no shell and no package manager, so a
# compromised cache node has nothing to pivot with.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/kilncache /usr/local/bin/kilncache

# The data directory is a volume mount point. It is created by the entrypoint at
# runtime rather than baked in, because distroless has no mkdir and the node
# creates its own tree on startup.
ENV KILNCACHE_DATA_DIR=/var/lib/kilncache \
    KILNCACHE_LISTEN=:8080 \
    KILNCACHE_LOG_FORMAT=json

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/kilncache"]
