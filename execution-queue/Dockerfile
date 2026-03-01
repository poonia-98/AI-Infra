FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/execution-queue ./main.go

FROM alpine:3.20
RUN adduser -D -H -u 10001 appuser
COPY --from=builder /out/execution-queue /usr/local/bin/execution-queue
EXPOSE 8096
USER appuser
ENTRYPOINT ["/usr/local/bin/execution-queue"]
