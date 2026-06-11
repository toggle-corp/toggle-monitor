# Multi-stage build: a Go build stage, then a distroless final image.
# Output is a static linux/amd64 binary at /app/toggle-monitor.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache deps separately from source for warmer rebuilds.
COPY go.mod go.sum ./
RUN go mod download

# Explicit COPYs (not `COPY .`) so the build cannot pick up secrets or
# build junk even if .dockerignore is deleted. Both files would have to
# drift before something leaks.
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# VERSION is the release stamp burned into the binary (read by main.version,
# emitted as the Sentry release on every event). Defaults to "dev" so
# `docker build` works without --build-arg; CI/CD overrides with the tag.
ARG VERSION=dev
# CGO disabled → fully static binary; trimpath strips build-host paths
# from the binary so it's reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/toggle-monitor ./cmd/toggle-monitor

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/toggle-corp/toggle-monitor"
LABEL org.opencontainers.image.description="Kubernetes-native uptime + SSL monitor with Slack alerts and ingress auto-discovery"
LABEL org.opencontainers.image.licenses="MIT"
# 65532 is the nonroot UID baked into the distroless image.
USER 65532:65532
WORKDIR /app
COPY --from=build /out/toggle-monitor /app/toggle-monitor

EXPOSE 8080
ENTRYPOINT ["/app/toggle-monitor"]
CMD ["serve", "--config", "/etc/toggle-monitor/config.yaml", "--listen", ":8080"]
