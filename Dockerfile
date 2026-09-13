# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first so source changes keep this layer cached.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binary: CGO disabled keeps the runtime image distroless-small.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bellwether .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/bellwether /bellwether

USER nonroot
EXPOSE 3000

# Distroless ships no wget or shell, so the binary probes its own
# /health endpoint; the port default matches the app's.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/bellwether", "healthcheck"]

# The Go binary is PID 1: it handles SIGTERM/SIGINT itself and spawns
# no children, so no init process is needed.
ENTRYPOINT ["/bellwether"]
