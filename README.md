# 🤖 AI Agent Infrastructure Platform

> A production-grade, containerised orchestration system for deploying, running, and monitoring autonomous AI agents — think Kubernetes, but purpose-built for agent workloads.

[![CI Pipeline](https://github.com/poonia-98/AI-Infra/actions/workflows/ci.yml/badge.svg)](https://github.com/poonia-98/AI-Infra/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go 1.24](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go)](https://golang.org)
[![Python 3.11](https://img.shields.io/badge/Python-3.11-3776AB?logo=python)](https://python.org)
[![Next.js 14](https://img.shields.io/badge/Next.js-14-000000?logo=nextdotjs)](https://nextjs.org)
[![Docker](https://img.shields.io/badge/Docker-Compose-2496ED?logo=docker)](https://docker.com)

---

## 📋 Table of Contents

- [Overview](#-overview)
- [Architecture](#-architecture)
- [Key Features](#-key-features)
- [Technology Stack](#-technology-stack)
- [Prerequisites](#-prerequisites)
- [Quick Start](#-quick-start)
- [Project Structure](#-project-structure)
- [API Reference](#-api-reference)
- [Testing](#-testing)
- [CI/CD](#-cicd)
- [Observability](#-observability)
- [Security](#-security)
- [Roadmap](#-roadmap)
- [Contributing](#-contributing)
- [License](#-license)

---

## 🌟 Overview

The AI Agent Infrastructure Platform provides a complete control plane for autonomous AI agents. It handles the full lifecycle from deployment to teardown, streaming live telemetry to a real-time dashboard while keeping a full audit trail of every execution.

```
┌─────────────────────────────────────────────────────────────────┐
│                     Developer / Operator                        │
│                        Browser UI                               │
└───────────────────────────┬─────────────────────────────────────┘
                            │ HTTP / WebSocket
┌───────────────────────────▼─────────────────────────────────────┐
│                      Next.js Frontend                           │
│              Dashboard · Logs · Metrics · Events                │
└──────────┬────────────────────────────────────┬─────────────────┘
           │ REST                               │ WebSocket
┌──────────▼──────────┐             ┌───────────▼─────────────────┐
│   FastAPI Backend   │             │     WebSocket Gateway (Go)  │
│  Agent Management   │             │   NATS → WebSocket Bridge   │
│  Execution Tracking │             └───────────┬─────────────────┘
│  Logs · Metrics     │                         │
└──────────┬──────────┘             ┌───────────▼─────────────────┐
           │                        │         NATS                │
           │                        │    Event Bus / PubSub       │
┌──────────▼──────────┐             └──┬──────────┬──────────┬────┘
│    PostgreSQL       │                │          │          │
│  Agents · Logs      │     ┌──────────▼──┐  ┌───▼──────┐  ┌▼──────────────┐
│  Executions         │     │  Executor   │  │ Metrics  │  │    Event      │
│  Metrics            │     │   (Go)      │  │Collector │  │   Processor   │
└─────────────────────┘     │ Docker Mgmt │  │  (Go)    │  │    (Go)       │
                            └──────┬──────┘  └───┬──────┘  └───────────────┘
┌─────────────────────┐            │              │
│       Redis         │     ┌──────▼──────────────▼──────────────┐
│  Cache · Buffers    │     │           Docker Engine             │
└─────────────────────┘     │   Agent Container 1 | 2 | 3 | N    │
                            └─────────────────────────────────────┘
                            ┌─────────────────────────────────────┐
                            │    Prometheus  │  Grafana            │
                            │    Metrics     │  Dashboards         │
                            └─────────────────────────────────────┘
```

---

## 🏗️ Architecture

### Component Map

```
platform/
├── backend/              ← Python/FastAPI control plane
├── executor/             ← Go service: Docker container lifecycle
├── metrics-collector/    ← Go service: CPU/mem metrics → NATS + Prometheus
├── event-processor/      ← Go service: NATS event consumer
├── websocket-gateway/    ← Go service: NATS → WebSocket bridge
├── frontend/             ← Next.js real-time dashboard
├── agent-runtime/        ← Python LangGraph agent (example workload)
└── docker-compose.yml    ← Full stack orchestration
```

### Data Flow

```
Agent Action (start/stop)
        │
        ▼
  FastAPI Backend  ──────────────────────► PostgreSQL
        │                                  (persist state)
        │ publish event
        ▼
      NATS
   ┌────┴──────────────────────┐
   │                           │
   ▼                           ▼
Executor                WebSocket Gateway
(manages Docker)        (push to browser)
   │
   ▼
Docker Container
(agent workload)
   │
   ▼
Metrics Collector
(every 5 seconds)
   │
   ├──► NATS (live metrics)
   └──► Prometheus /metrics
```

### Service Responsibilities

| Service | Language | Port | Responsibility |
|---------|----------|------|----------------|
| **Backend** | Python/FastAPI | 8000 | REST API, agent management, DB persistence, auth |
| **Executor** | Go | 8081 | Docker container create/start/stop/delete, log streaming |
| **Metrics Collector** | Go | 8082 | Docker stats → NATS + Prometheus every 5s |
| **Event Processor** | Go | 8083 | NATS consumer for long-term storage and alerting |
| **WebSocket Gateway** | Go | 8084 | NATS → WebSocket bridge for frontend live updates |
| **Frontend** | Next.js | 3000 | Real-time dashboard with graphs, logs, event feed |
| **NATS** | - | 4222 | Lightweight event bus |
| **PostgreSQL** | - | 5432 | Persistent store for all entities |
| **Redis** | - | 6379 | Cache and live log buffer |
| **Prometheus** | - | 9090 | Metrics scraping and storage |
| **Grafana** | - | 3001 | Metrics dashboards |

---

## ✨ Key Features

### Agent Lifecycle Management
- Create agents with custom Docker images, resource limits, environment variables
- Start, stop, restart, and delete with full state tracking
- States: `created` → `running` → `stopped` / `failed`
- Auto-restart on failure with configurable retry count

### Real-Time Observability
- Live CPU and memory graphs streamed via WebSocket
- Per-agent sparklines on the dashboard
- System-wide aggregate metrics (avg CPU, avg mem, events/s, metrics/s)
- Event feed showing all state transitions in real time

### Execution Tracking
- Full audit trail: every `start` creates an `Execution` record
- Tracks start time, end time, duration, exit code, and output
- Linked logs for each execution

### Metrics Pipeline
```
Docker Stats API → Metrics Collector → NATS (live)
                                     → Prometheus (historical)
                                     → PostgreSQL (queryable)
```

### Event-Driven Architecture
All state changes publish to NATS subjects:
```
agents.events     ← agent.started, agent.stopped, agent.failed, agent.heartbeat
agents.metrics    ← CPU/mem snapshots every 5s
agents.logs       ← log lines from running containers
```

### Security Hardening
- Security headers: `X-Content-Type-Options`, `X-Frame-Options`, `X-XSS-Protection`, `Cross-Origin-Resource-Policy`
- Input validation via Pydantic on all endpoints
- OWASP ZAP scanned: 0 HIGH, 0 MEDIUM findings

---

## 🛠️ Technology Stack

| Layer | Technologies |
|-------|-------------|
| **Backend API** | Python 3.11, FastAPI, SQLAlchemy (async), Pydantic v2, Alembic |
| **Go Services** | Go 1.24, Docker SDK v27, NATS.go, Gin, Prometheus client |
| **Frontend** | Next.js 14, TypeScript, Tailwind CSS, Recharts, WebSocket API |
| **Messaging** | NATS (JetStream optional) |
| **Databases** | PostgreSQL 15, Redis 7 |
| **Observability** | Prometheus, Grafana, OpenTelemetry, structlog |
| **Containers** | Docker Engine, Docker Compose v2 |
| **Testing** | pytest, Jest, k6, OWASP ZAP, Pumba |
| **CI/CD** | GitHub Actions |

---

## 📋 Prerequisites

- **Docker** ≥ 24.x and **Docker Compose** v2.x
- **Git**
- *(Optional, for local dev)* Node.js 20+, Go 1.24+, Python 3.11+

---

## 🚀 Quick Start

### 1. Clone the repository

```bash
git clone https://github.com/poonia-98/AI-Infra.git
cd AI-Infra/platform
```

### 2. Start all services

```bash
docker compose up -d
```

Wait for everything to become healthy:

```bash
docker compose ps
# All services should show "healthy" or "running"
```

### 3. Open the dashboard

```
http://localhost:3000
```

### 4. Deploy your first agent

```bash
# Create an agent
curl -X POST http://localhost:8000/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{
    "name": "hello-world",
    "image": "alpine:latest",
    "agent_type": "langgraph",
    "config": {"command": "echo Hello from my first agent"}
  }'

# Start it (replace <id> with the returned id)
curl -X POST http://localhost:8000/api/v1/agents/<id>/start

# Check its logs
curl http://localhost:8000/api/v1/agents/<id>/logs?limit=20

# Stop and clean up
curl -X POST http://localhost:8000/api/v1/agents/<id>/stop
curl -X DELETE http://localhost:8000/api/v1/agents/<id>
```

Or use the dashboard: click **DEPLOY AGENT**, fill in the form, and click **Start**.

---

## 📁 Project Structure

```
platform/
├── backend/
│   ├── main.py                 ← FastAPI app, middleware, lifespan
│   ├── models.py               ← SQLAlchemy ORM models
│   ├── schemas.py              ← Pydantic request/response schemas
│   ├── database.py             ← Async engine and session factory
│   ├── config.py               ← Environment-based settings
│   ├── routers/
│   │   ├── agents.py           ← Agent CRUD + lifecycle endpoints
│   │   └── data.py             ← Logs, metrics, events, alerts, containers
│   ├── services/
│   │   ├── agent_service.py    ← Business logic for agent management
│   │   ├── alert_service.py    ← Alert rule evaluation
│   │   ├── nats_service.py     ← NATS pub/sub wrapper
│   │   └── redis_service.py    ← Redis cache wrapper
│   └── tests/
│       └── test_agents.py
│
├── executor/
│   └── main.go                 ← Docker container lifecycle manager
│
├── metrics-collector/
│   └── main.go                 ← Docker stats → NATS + Prometheus
│
├── event-processor/
│   └── main.go                 ← NATS consumer for events
│
├── websocket-gateway/
│   └── main.go                 ← NATS → WebSocket bridge
│
├── frontend/
│   └── src/app/
│       ├── page.tsx            ← Main dashboard
│       └── agents/[id]/        ← Agent detail page
│
├── agent-runtime/
│   └── agent.py                ← LangGraph example agent
│
├── docker-compose.yml
└── .github/
    └── workflows/
        └── ci.yml              ← GitHub Actions CI pipeline
```

---

## 📚 API Reference

Full OpenAPI spec: `http://localhost:8000/openapi.json`

Interactive docs: `http://localhost:8000/docs`

### Agents

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/agents` | List all agents |
| `POST` | `/api/v1/agents` | Create a new agent |
| `GET` | `/api/v1/agents/{id}` | Get agent details |
| `PUT` | `/api/v1/agents/{id}` | Update agent config |
| `DELETE` | `/api/v1/agents/{id}` | Delete agent |
| `POST` | `/api/v1/agents/{id}/start` | Start agent container |
| `POST` | `/api/v1/agents/{id}/stop` | Stop agent container |
| `POST` | `/api/v1/agents/{id}/restart` | Restart agent |

### Data

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/api/v1/logs` | List logs (filterable by level, agent, time) |
| `GET` | `/api/v1/events` | List events |
| `GET` | `/api/v1/metrics` | List metric snapshots |
| `GET` | `/api/v1/metrics/timeseries` | Time-bucketed metric series |
| `GET` | `/api/v1/executions` | List executions |
| `GET` | `/api/v1/alerts/rules` | List alert rules |
| `POST` | `/api/v1/alerts/rules` | Create alert rule |
| `GET` | `/api/v1/containers` | List container records |

### WebSocket

```
ws://localhost:8084/ws
```

Messages are JSON with `type` field: `agent.started`, `agent.stopped`, `agent.heartbeat`, `metric.snapshot`, `log.line`

---

## 🧪 Testing

### Unit Tests

```bash
# Backend
cd backend && pytest tests/ -v

# Frontend
cd frontend && npm test -- --ci --coverage

# Go services
cd executor && go build ./...
cd metrics-collector && go build ./...
cd event-processor && go build ./...
cd websocket-gateway && go build ./...
```

### Integration Tests

```bash
docker compose up -d --wait

# Full agent lifecycle smoke test
curl -X POST http://localhost:8000/api/v1/agents \
  -H "Content-Type: application/json" \
  -d '{"name":"ci-agent","image":"alpine:latest","agent_type":"langgraph","config":{}}' \
  | tee /tmp/agent.json

AGENT_ID=$(jq -r .id /tmp/agent.json)
curl -X POST http://localhost:8000/api/v1/agents/$AGENT_ID/start
sleep 5
curl http://localhost:8000/api/v1/agents/$AGENT_ID/logs?limit=5
curl -X DELETE http://localhost:8000/api/v1/agents/$AGENT_ID
```

### Performance Tests (k6)

```bash
# Baseline read-only test — 100 VUs
k6 run baseline-test.js -e BASE_URL=http://localhost:8000/api/v1

# Agent lifecycle test — 15 VUs
k6 run lifecycle-test.js -e BASE_URL=http://localhost:8000/api/v1
```

Expected results:
- Read endpoints: p95 < 500ms at 100 concurrent users
- Container lifecycle: p95 < 10s at 15 concurrent users (Docker overhead)

### Security Tests (OWASP ZAP)

```bash
# Passive baseline scan
docker run --network platform_network \
  -v "$(pwd)/zap-reports:/zap/wrk/:rw" \
  ghcr.io/zaproxy/zaproxy:stable \
  zap-baseline.py -t http://backend:8000 -r baseline.html

# Active API scan
docker run --network platform_network \
  -v "$(pwd)/zap-reports:/zap/wrk/:rw" \
  ghcr.io/zaproxy/zaproxy:stable \
  zap-api-scan.py -t http://backend:8000/openapi.json -f openapi -a -r full_scan.html
```

Current scan results: **0 FAIL, 5 WARN (Low severity only)**

### Chaos Engineering (Pumba)

```bash
# Kill executor mid-run to test recovery
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  gaiaadm/pumba kill --signal SIGKILL platform-executor-1

# Inject 3s network delay on backend
docker run --rm \
  -v /var/run/docker.sock:/var/run/docker.sock \
  gaiaadm/pumba netem --duration 1m delay --time 3000 platform-backend-1
```

---

## 🔁 CI/CD

Every push to `main`, `develop`, or `master` runs:

```
┌─────────────────────┐    ┌──────────────────────┐    ┌─────────────────────┐
│  backend-unit-tests │    │ frontend-unit-tests   │    │   go-build-check    │
│  pytest tests/ -v   │    │ npm test --coverage   │    │ go build ./...      │
│                     │    │                       │    │ (all 4 Go services) │
└──────────┬──────────┘    └──────────┬────────────┘    └──────────┬──────────┘
           │                          │                            │
           └──────────────────────────┼────────────────────────────┘
                                      │
                          ┌───────────▼────────────┐
                          │   integration-tests    │
                          │ docker compose up -d   │
                          │ full lifecycle smoke   │
                          └────────────────────────┘
```

Performance tests run on manual trigger (`workflow_dispatch`) or schedule.

---

## 📊 Observability

### Metrics

Prometheus scrapes two endpoints:
- `http://backend:8000/metrics` — API request counts, latencies
- `http://metrics-collector:8082/metrics` — per-container CPU, memory, network I/O

Access Prometheus at `http://localhost:9090`

### Grafana

Pre-configured dashboards at `http://localhost:3001`
- Login: `admin` / `admin`
- Dashboards: Agent Overview, System Metrics, Request Rates

### Logs

All services log structured JSON to stdout:

```bash
# Follow all service logs
docker compose logs -f

# Single service
docker logs -f platform-backend-1
```

### Health Endpoints

| Service | Health Check |
|---------|-------------|
| Backend | `GET http://localhost:8000/health` |
| Executor | `GET http://localhost:8081/health` |
| Metrics Collector | `GET http://localhost:8082/health` |
| WebSocket Gateway | `GET http://localhost:8084/health` |

---

## 🔒 Security

### Headers Applied to All Responses

```
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-XSS-Protection: 1; mode=block
Cross-Origin-Resource-Policy: cross-origin
Referrer-Policy: strict-origin-when-cross-origin
```

### Input Validation

All API inputs validated via Pydantic v2 — invalid inputs return `422 Unprocessable Entity`, never `500`.

### Network Isolation

All services run on an isolated Docker network (`platform_network`). Only ports `3000`, `8000`, and `8084` are exposed to the host.

### OWASP ZAP Scan Results

| Severity | Count | Status |
|----------|-------|--------|
| HIGH | 0 | ✅ Clean |
| MEDIUM | 0 | ✅ Clean |
| LOW | 5 | ⚠️ Accepted |
| INFORMATIONAL | — | ℹ️ |

---

## 📈 Roadmap

- [x] Agent lifecycle (create/start/stop/delete)
- [x] Real-time metrics and logs via WebSocket
- [x] Prometheus + Grafana integration
- [x] NATS event bus
- [x] Security headers and input validation
- [x] CI/CD pipeline with GitHub Actions
- [x] OWASP ZAP security scanning
- [x] k6 performance testing
- [x] Chaos engineering with Pumba
- [ ] Full log streaming from executor to frontend
- [ ] Kubernetes operator for hybrid deployments
- [ ] Multi-tenancy and RBAC
- [ ] Webhook integrations for external events
- [ ] Helm charts for Kubernetes deployment
- [ ] Distributed tracing (OpenTelemetry)
- [ ] Alert notifications (Slack, PagerDuty)

---

## 🤝 Contributing

Contributions are welcome!

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/my-feature`
3. Make your changes and add tests
4. Run the test suite: `pytest tests/ -v && npm test && go build ./...`
5. Submit a pull request

Please follow conventional commit messages: `feat:`, `fix:`, `docs:`, `chore:`.

---

## 📄 License

This project is licensed under the **Apache License 2.0** — see the [LICENSE](LICENSE) file for details.

---

## 💬 Community

- [GitHub Issues](https://github.com/poonia-98/AI-Infra/issues) — report bugs or request features
- [Discussions](https://github.com/poonia-98/AI-Infra/discussions) — ask questions and share ideas

---

Built with ❤️ by developers who believe AI agents deserve great infrastructure.