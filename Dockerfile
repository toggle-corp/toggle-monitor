# Multi-stage build: a Go build stage, then a distroless final image.
# Output is a static linux/amd64 binary at /app/toggle-monitor.

FROM golang:1.26-alpine AS build
WORKDIR /src
# Cache deps separately from source for warmer rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO disabled → fully static binary; trimpath strips build-host paths
# from the binary so it's reproducible.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags '-s -w' \
    -o /out/toggle-monitor ./cmd/toggle-monitor

FROM gcr.io/distroless/static-debian12:nonroot
# 65532 is the nonroot UID baked into the distroless image.
USER 65532:65532
WORKDIR /app
COPY --from=build /out/toggle-monitor /app/toggle-monitor

EXPOSE 8080
ENTRYPOINT ["/app/toggle-monitor"]
CMD ["serve", "--config", "/etc/toggle-monitor/config.yaml", "--listen", ":8080"]
