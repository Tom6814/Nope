# --- Stage 1: build the frontend ---
# Vite outputs to ../internal/web/dist (repo-relative internal/web/dist),
# which is embedded into the Go binary.
# NOTE: use a glibc base (bookworm-slim), NOT alpine. vite-plus (rolldown)
# ships native bindings via optionalDependencies; the musl (alpine) variant
# is not reliably installed, causing "Cannot find module 'vite-plus.linux-x64-musl.node'".
# bookworm-slim matches the CI build environment (ubuntu, node 22).
FROM node:22-bookworm-slim AS web-builder
WORKDIR /app
# Pin pnpm explicitly instead of relying on corepack's on-demand download,
# which can fail in restricted networks. Build scripts are skipped during
# install (--ignore-scripts): the container has no .git so the husky prepare
# hook is useless anyway; `pnpm build` below runs the real build script.
RUN npm install -g pnpm@10.32.1

# Dependency layer first for Docker layer caching: only the manifests change
# rarely, so reinstalling deps is skipped unless they change.
COPY web/package.json web/pnpm-lock.yaml ./web/
WORKDIR /app/web
RUN pnpm install --frozen-lockfile --ignore-scripts --fetch-retries=5 --fetch-timeout=120000

# Full source, then the production build (tsc && vite). NODE_OPTIONS raises
# the heap ceiling so large projects don't OOM on memory-limited hosts.
COPY web/ ./
ENV NODE_OPTIONS=--max-old-space-size=4096
RUN pnpm build

# --- Stage 2: build the backend ---
# CGO is required (provider/ilink bundles silk C code), hence gcc.
# SQLite is modernc (pure Go), no extra system deps needed for it.
FROM golang:1.25-bookworm AS go-builder
RUN apt-get update && apt-get install -y --no-install-recommends gcc && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Frontend assets are embedded via go:embed, so they must come from the
# web-builder stage (NOT from the local build context) to stay in sync.
COPY --from=web-builder /app/internal/web/dist ./internal/web/dist
ENV CGO_ENABLED=1
RUN go build -trimpath -ldflags="-s -w" -o /out/openilink-hub .

# --- Stage 3: runtime ---
# The cgo binary links against glibc, so a glibc base is required.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
COPY --from=go-builder /out/openilink-hub /usr/local/bin/openilink-hub
# Running as root: config.DataDir() resolves to /var/lib/openilink-hub
# (default SQLite path: /var/lib/openilink-hub/openilink.db).
ENV LISTEN=:9800
EXPOSE 9800
VOLUME ["/var/lib/openilink-hub"]
WORKDIR /app
CMD ["openilink-hub"]
