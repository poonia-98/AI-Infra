FROM golang:1.21-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git ca-certificates
COPY go.mod .
COPY . .
RUN go mod download && go mod tidy && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o event-processor .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates curl tzdata
WORKDIR /app
COPY --from=builder /app/event-processor /app/event-processor
EXPOSE 8082
ENTRYPOINT ["/app/event-processor"]