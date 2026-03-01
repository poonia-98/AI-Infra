FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git ca-certificates
WORKDIR /app
COPY go.mod ./
RUN GONOSUMDB=* GOTOOLCHAIN=local go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o node-manager .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl
COPY --from=builder /app/node-manager /node-manager
ENTRYPOINT ["/node-manager"]