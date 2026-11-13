# Cross-compiles on the build machine's own platform (no QEMU emulation):
# Go builds the binary for TARGETARCH, the final image is just that binary.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o saic-logger .

FROM scratch
COPY --from=builder /app/saic-logger .
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=3 \
    CMD ["/saic-logger", "--health"]
ENTRYPOINT ["/saic-logger"]
