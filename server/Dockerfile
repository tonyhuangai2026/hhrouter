# ---- Build stage ----
# Compiles the Agent Router Platform backend into a single static binary.
FROM golang:1.23-alpine AS build
WORKDIR /src

# Cache module downloads first for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

# Build the server binary. CGO is disabled so the binary runs on a minimal
# (scratch/alpine) runtime without libc.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server .

# ---- Runtime stage ----
# Minimal alpine image; ca-certificates lets the relay reach HTTPS upstreams.
FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/server /app/server

# PORT is read by config.Load(); default 3000. Override via env if desired.
ENV PORT=3000
EXPOSE 3000
USER app

# A simple in-image healthcheck against the liveness endpoint.
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=10 \
    CMD wget -qO- "http://127.0.0.1:${PORT}/api/ping" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/server"]
