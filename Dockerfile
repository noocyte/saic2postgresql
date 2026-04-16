FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o saic-logger .

FROM scratch
COPY --from=builder /app/saic-logger .
ENTRYPOINT ["/saic-logger"]
