FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY go.mod ./
RUN GONOSUMDB=* GOTOOLCHAIN=local go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o k8s-executor .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /app/k8s-executor /k8s-executor
ENTRYPOINT ["/k8s-executor"]