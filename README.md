# AI Agent Infrastructure Platform

## Quick Start

```bash
# 1. Build agent runtime image
docker build -t agent-runtime:latest ./agent-runtime

# 2. Start all services
docker-compose up -d

# Or use make:
make setup
```

## URLs

| Service | URL |
|---------|-----|
| Frontend | http://localhost:3000 |
| API Docs | http://localhost:8000/docs |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3001 (admin/admin) |
| NATS Monitor | http://localhost:8222 |

## Service Ports

| Service | Port |
|---------|------|
| Backend (FastAPI) | 8000 |
| Executor (Go) | 8081 |
| Event Processor (Go) | 8082 |
| Metrics Collector (Go) | 8083 |
| WebSocket Gateway (Go) | 8084 |
| Frontend (Next.js) | 3000 |
| PostgreSQL | 5432 |
| Redis | 6379 |
| NATS | 4222 |
| Prometheus | 9090 |
| Grafana | 3001 |

## Commands

```bash
make up          # Start all
make down        # Stop all
make logs        # View logs
make status      # Check health
make clean       # Remove all + volumes
```