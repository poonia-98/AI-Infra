FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app

# Copy EVERYTHING first (important)
COPY . .

# Now resolve dependencies
RUN GONOSUMDB=* GOTOOLCHAIN=local go mod tidy

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-w -s" -o scheduler-service main.go


FROM alpine:3.19

RUN apk add --no-cache ca-certificates curl

WORKDIR /

COPY --from=builder /app/scheduler-service /scheduler-service

ENTRYPOINT ["/scheduler-service"]