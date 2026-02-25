FROM golang:1.24-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git ca-certificates
COPY go.mod .
COPY . .
RUN GOTOOLCHAIN=auto GOFLAGS=-mod=mod go mod download && GOTOOLCHAIN=auto GOFLAGS=-mod=mod go mod tidy && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o executor .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl tzdata
WORKDIR /app
COPY --from=builder /app/executor /app/executor
EXPOSE 8081
ENTRYPOINT ["/app/executor"]