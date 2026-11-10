FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o saic-logger .

FROM scratch
COPY --from=builder /app/saic-logger .
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=3 \
    CMD ["/saic-logger", "--health"]
ENTRYPOINT ["/saic-logger"]
