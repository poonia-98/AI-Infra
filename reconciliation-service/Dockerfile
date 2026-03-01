FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install git (required for go modules sometimes)
RUN apk add --no-cache git

# Copy everything
COPY . .

# Clean and download modules
RUN go mod tidy

# Build binary
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o reconciliation-service main.go


FROM alpine:3.19

WORKDIR /app

# Copy binary
COPY --from=builder /app/reconciliation-service .

# Create non-root user
RUN adduser -D appuser
USER appuser

# Run service
CMD ["./reconciliation-service"]