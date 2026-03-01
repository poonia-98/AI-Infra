FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY . .
RUN GONOSUMDB=* GOFLAGS=-mod=mod GOTOOLCHAIN=local go mod tidy
RUN CGO_ENABLED=0 GOOS=linux GONOSUMDB=* GOFLAGS=-mod=mod go build -ldflags="-w -s" -o service .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /app/service /service
ENTRYPOINT ["/service"]