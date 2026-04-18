FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o saic-logger .

FROM alpine:3.20
RUN apk add --no-cache wget
COPY --from=builder /app/saic-logger .
HEALTHCHECK --interval=60s --timeout=5s --start-period=120s --retries=3 \
    CMD wget -qO- http://localhost:8080/healthz || exit 1
ENTRYPOINT ["/saic-logger"]
