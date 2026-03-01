# AgentPlane

AgentPlane is an infrastructure platform for deploying and running autonomous AI agents at scale. Think of it the way you think about Kubernetes — you don't manually manage where your containers run, how many replicas spin up, or what happens when a node dies. AgentPlane does the same thing, but for agents.

You give it an agent. It handles the rest.

---

## What it actually does

Most agent frameworks stop at the prompt. You write a system prompt, wire up some tools, and ship it. That works fine until you have 50 agents running in production, one of them starts misbehaving at 3am, you need to debug exactly what decision it made at step 14 of execution #847, and you're staring at logs wondering what happened.

AgentPlane was built around the operational reality of running agents in production:

- Agents need to be deployed somewhere, scaled when load increases, and restarted when they crash
- Agents need memory that persists across sessions — not just a context window, but real retrieval
- Agents talking to other agents across organizations need trust boundaries and billing
- You need to be able to replay any execution and see every single step
- Sometimes you want to mutate an agent's prompt, run it against a fitness function, and automatically promote the version that performs better

The architecture reflects these needs directly. There isn't a "monitoring addon" bolted on — observability is wired into every service from the start.

---

## Stack

**Backend:** Python 3.11 / FastAPI — handles API, auth, billing, and orchestration logic  
**Microservices:** Go 1.21 — the 28 Go services handle the actual runtime work  
**Frontend:** Next.js 14 (App Router) — the control plane UI  
**Database:** PostgreSQL 16 with pgvector extension  
**Cache:** Redis 7  
**Event bus:** NATS JetStream  
**Observability:** Prometheus + Grafana + OpenTelemetry

---

## Getting started

You need Docker and Docker Compose v2. That's it.

```bash
git clone https://github.com/your-org/agentplane
cd agentplane
pip install -e cli/
agentplane install
```

The install command will generate your `.env` with random secrets, pull images, run all 10 migrations, seed a demo agent, and open the dashboard. First run takes a few minutes while images download. After that, `agentplane up` starts everything in under 30 seconds.

Once running:

| URL | What's there |
|-----|-------------|
| http://localhost:3000 | Control plane dashboard |
| http://localhost:8000/docs | API documentation |
| http://localhost:8200 | Control Brain API |
| http://localhost:3001 | Grafana (admin/admin) |
| http://localhost:9090 | Prometheus |

---

## CLI reference

```bash
agentplane install       # First-time setup — generates secrets, runs migrations, seeds data
agentplane up            # Start everything
agentplane down          # Stop everything
agentplane restart       # Stop then start
agentplane status        # Live health table of all 35 services
agentplane logs backend  # Tail logs for a specific service
agentplane migrate       # Run pending SQL migrations
agentplane deploy-agent  # Interactive wizard to deploy an agent
agentplane brain         # Show Control Brain state and agent statuses
agentplane services      # List all registered microservices and their health
agentplane reset         # Nuclear option — wipes volumes, starts fresh
agentplane dashboard     # Open the browser
```

---

## Project structure

```
agentplane/
│
├── backend/                    # FastAPI application — the main API
│   ├── main.py                 # App entrypoint, all routers registered here
│   ├── config.py               # Settings via pydantic-settings
│   ├── database.py             # Async SQLAlchemy setup
│   ├── models.py               # ORM models
│   ├── routers/
│   │   ├── agents.py           # Core agent CRUD and lifecycle
│   │   ├── data.py             # Logs, events, metrics, executions
│   │   ├── infrastructure.py   # Schedules, nodes
│   │   ├── platform_ops.py     # Config, secrets, audit, queue
│   │   ├── time_travel.py      # Execution replay API
│   │   ├── brain.py            # Proxy to Control Brain service
│   │   ├── enterprise/
│   │   │   ├── auth.py         # JWT auth, SSO
│   │   │   ├── organisations.py
│   │   │   └── platform.py     # Workflows, versions, memory, marketplace
│   │   └── cloud/
│   │       ├── billing.py
│   │       ├── observability.py
│   │       ├── sandbox.py
│   │       ├── simulation.py
│   │       ├── regions.py
│   │       ├── federation.py
│   │       ├── evolution.py
│   │       ├── knowledge.py
│   │       └── vault.py
│   ├── services/
│   │   ├── agent_service.py    # Core agent business logic
│   │   ├── brain_service.py    # HTTP client to Control Brain
│   │   ├── cache_service.py    # Redis cache layer (replaces DB polling)
│   │   ├── memory_service.py   # pgvector semantic memory
│   │   ├── nats_service.py     # NATS connection and subscriptions
│   │   ├── redis_service.py    # Redis connection and helpers
│   │   └── ...
│   └── middleware/
│       ├── audit.py            # Every write goes to audit_logs
│       └── rate_limit.py       # Per-org rate limiting via Redis
│
├── control-brain/              # Go — Central orchestration layer
├── executor/                   # Go — Docker-based agent execution
├── k8s-executor/               # Go — Kubernetes-based execution
├── memory-vector-engine/       # Go — pgvector semantic memory
├── agent-autoscaler/           # Go — Scale agents based on queue depth
├── agent-gateway/              # Go — Agent-to-agent routing
├── agent-sandbox-manager/      # Go — Isolation policy enforcement
├── agent-simulation-engine/    # Go — Synthetic workloads and chaos testing
├── agent-evolution-engine/     # Go — Genetic algorithm optimization
├── agent-federation-network/   # Go — Cross-org agent collaboration
├── agent-observability/        # Go — Distributed tracing and lineage
├── billing-engine/             # Go — Plans, invoices, usage
├── subscription-manager/       # Go — Plan management
├── usage-meter/                # Go — Real-time resource metering
├── secrets-manager/            # Go — AES-256-GCM vault
├── marketplace-service/        # Go — Agent marketplace
├── marketplace-validator/      # Go — Publish validation
├── global-scheduler/           # Go — Multi-region placement
├── region-controller/          # Go — Regional agent management and failover
├── cluster-controller/         # Go — Cluster lifecycle
├── workflow-engine/            # Go — DAG workflow execution
├── event-processor/            # Go — NATS event fan-out
├── log-processor/              # Go — Batched log ingestion
├── metrics-collector/          # Go — Container metrics scraping
├── websocket-gateway/          # Go — Real-time push to UI
├── reconciliation-service/     # Go — Drift detection and self-healing
├── scheduler-service/          # Go — Cron and trigger scheduling
├── node-manager/               # Go — Node heartbeat and capacity
├── agent-runtime/              # Base agent container image
│
├── frontend/                   # Next.js 14 control plane UI
│   └── src/
│       ├── app/
│       │   ├── (auth)/login/   # Login page with boot animation
│       │   └── (dashboard)/
│       │       ├── page.tsx        # Overview dashboard
│       │       ├── agents/         # Agent list and detail
│       │       ├── brain/          # Control Brain state viewer
│       │       ├── services/       # Service registry
│       │       ├── memory/         # Agent memory browser
│       │       ├── time-travel/    # Execution replay debugger
│       │       ├── federation/     # Federation peer management
│       │       ├── evolution/      # Experiment viewer
│       │       ├── simulation/     # Simulation environments
│       │       ├── sandbox/        # Isolation policies
│       │       ├── observability/  # Traces and performance
│       │       ├── regions/        # Region and failover view
│       │       ├── billing/        # Plans and invoices
│       │       ├── vault/          # Secrets management
│       │       └── knowledge/      # Knowledge base
│       ├── components/
│       │   └── Sidebar.tsx     # Navigation with 6 sections
│       ├── contexts/
│       │   └── AuthContext.tsx # JWT auth context
│       ├── lib/
│       │   ├── api.ts          # All API client methods
│       │   └── auth.ts         # Token management (memory-only, no localStorage)
│       └── middleware.ts       # Route protection
│
├── migrations/                 # SQL migration files
│   ├── init.sql
│   ├── 0004_production_infrastructure.sql
│   ├── 0005_enterprise.sql
│   ├── 0006_global_platform.sql
│   ├── 0007_cloud_platform_extension.sql
│   ├── 0008_complete_cloud_platform.sql
│   ├── 0009_advanced_features.sql
│   └── 0010_platform_completion.sql
│
├── cli/
│   └── agentplane.py           # Single-file CLI tool
│
├── docs/
│   └── ARCHITECTURE.md         # Full architecture documentation
│
├── helm/                       # Kubernetes Helm charts
├── grafana/                    # Grafana dashboard definitions
├── prometheus/                 # Prometheus config
├── otel/                       # OpenTelemetry collector config
├── docker-compose.yml
└── .env.example
```

---

## How the services talk to each other

Everything runs inside a Docker network called `platform_network`. Services communicate over HTTP and via NATS for events.

The general flow:

1. A request comes into the **backend** API on port 8000
2. The backend either handles it directly or delegates to a Go service
3. State changes get published to NATS as events
4. Services that care about that event update their own state
5. The backend subscribes to `agents.events` and keeps Redis in sync — nothing polls the database for agent status

The **Control Brain** on port 8200 sits above all of this. It runs a reconciliation loop every 10 seconds — comparing what the database says should be running against what's actually running — and issues corrective commands when they diverge. Placement decisions also go through the brain: when you deploy an agent, it picks which node and region gets it based on current capacity.

```
Incoming request
    ↓
Backend API (:8000)
    ↓
Go service (executor, billing, etc.)
    ↓
NATS: agents.events
    ↓
Event processor fans out
    ↓
Redis cache updated — no DB polling
    ↓
WebSocket gateway pushes to UI
```

---

## Key features

### Control Brain

Every agent deployment, scale event, and failover goes through the Control Brain. It maintains in-memory state of the entire platform and reconciles against reality on a configurable interval. If a service registers and then stops heartbeating, the brain marks it degraded and can trigger failover. The brain's state is visible at `/api/v1/brain/state` and from the CLI with `agentplane brain`.

### Vector Memory

Agents can store memories that persist across executions. The memory engine uses pgvector's HNSW index for cosine similarity search over 1536-dimensional embeddings. Memories have types — episodic, semantic, procedural, working — and TTLs. Short-term memories expire after 24 hours by default. Semantic memories don't expire unless you set a TTL explicitly.

You need `OPENAI_API_KEY` set for embedding generation, or you can point `EMBEDDING_API_URL` at any OpenAI-compatible endpoint.

### Time Travel Debugger

Every execution step gets stored — the full prompt, the completion, the tool call and its result, timing, cost. The time travel UI lets you browse the timeline of any execution and click into any step for full detail. You can replay from any step, which creates a new execution branch. You can also edit the prompt at a specific step and see what would have happened differently. Useful both for development debugging and production post-mortems.

### Agent Evolution

The evolution engine runs a genetic algorithm over agent configurations. You create an experiment, define population size and mutation rate, and let it run. Each genome varies the base agent — different prompt phrasing, tool selection, workflow steps. Fitness is a weighted combination of task success rate, latency, and cost. Top performers survive to the next generation. After enough generations, promote the best genome as the new production agent version.

### Federation

Organizations can connect their AgentPlane instances as federation peers. A research agent in your deployment can delegate a summarization task to an agent running in a partner organization's deployment. Tasks are HMAC-signed. Trust levels — full, partial, sandboxed — control what the remote agent can access. The requesting org pays for the remote execution through a billing settlement system.

### Simulation Engine

Before pushing a new agent version to production, you can throw synthetic load at it. The simulation engine supports load tests, chaos scenarios that randomly kill agents mid-execution, tool latency injection, and region outage simulation. Results include p50/p95/p99 latency, failure rates, and estimated cost.

### Sandbox Isolation

Agents run inside one of three isolation modes depending on the configured policy: standard Docker namespaces, gVisor for syscall filtering, or Firecracker microVMs for maximum isolation. Sandbox policies define CPU and memory caps, network access rules, and permitted syscalls.

---

## Authentication

The frontend uses a two-token system. The access token lives in JavaScript memory only — never localStorage, never sessionStorage. The refresh token is stored as an HttpOnly cookie, so XSS attacks can't steal it. Route protection runs at two layers: Next.js middleware checks for the session cookie before the page loads, and the auth context does a secondary check client-side. Login has a 5-attempt lockout with a 30-second cooldown.

Service-to-service calls inside the platform use `X-Service-API-Key` headers, not user JWT tokens.

---

## Configuration

Copy `.env.example` to `.env` before first run. `agentplane install` does this automatically and replaces placeholder values with real random secrets. Things you'll likely want to set manually:

```bash
# Required for vector memory
OPENAI_API_KEY=sk-...

# Required for Stripe billing
STRIPE_SECRET_KEY=sk_live_...
STRIPE_WEBHOOK_SECRET=whsec_...

# Optional — SSO login
GOOGLE_CLIENT_ID=...
GITHUB_CLIENT_ID=...
```

Everything else has working defaults for local development.

---

## Running migrations manually

```bash
# Via CLI
agentplane migrate

# Directly via psql
docker compose exec -T postgres psql -U postgres -d agentdb < migrations/0010_platform_completion.sql
```

Migrations are plain SQL files numbered sequentially. They use `CREATE TABLE IF NOT EXISTS` throughout so running them more than once is safe.

---

## Adding a new agent

Via CLI:
```bash
agentplane deploy-agent
```

Via API:
```bash
curl -X POST http://localhost:8000/api/v1/agents \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <token>" \
  -d '{
    "name": "research-agent",
    "docker_image": "agent-runtime:latest",
    "cpu_limit": 1.0,
    "memory_limit": "512m",
    "env_vars": {
      "AGENT_ROLE": "research",
      "OPENAI_API_KEY": "sk-..."
    }
  }'
```

Via the dashboard: Agents → New Agent.

---

## Port reference

| Port | Service |
|------|---------|
| 3000 | Frontend dashboard |
| 3001 | Grafana |
| 4222 | NATS |
| 4317 | OTEL collector (gRPC) |
| 5432 | PostgreSQL |
| 6379 | Redis |
| 8000 | Backend API |
| 8081 | Executor |
| 8082 | Event processor |
| 8083 | Metrics collector |
| 8084 | WebSocket gateway |
| 8085 | Reconciliation service |
| 8086 | Scheduler service |
| 8087 | Node manager |
| 8088 | Log processor |
| 8092 | Workflow engine |
| 8093 | Agent autoscaler |
| 8094 | Agent gateway |
| 8095 | Marketplace service |
| 8096 | Agent sandbox manager |
| 8097 | Agent simulation engine |
| 8098 | Global scheduler |
| 8099 | Region controller |
| 8100 | Usage meter |
| 8101 | Billing engine |
| 8102 | Subscription manager |
| 8103 | Memory vector engine |
| 8104 | Secrets manager |
| 8105 | Marketplace validator |
| 8106 | Agent evolution engine |
| 8107 | Agent federation network |
| 8108 | Cluster controller |
| 8109 | Agent observability |
| 8200 | Control Brain |
| 9090 | Prometheus |

---

## Deploying to production

The platform ships with a Helm chart in `helm/` for Kubernetes deployments.

```bash
helm install agentplane ./helm/ai-platform \
  --namespace agentplane \
  --create-namespace \
  --set backend.secretKey="<your-secret>" \
  --set postgres.password="<your-password>" \
  --set openai.apiKey="<your-key>"
```

For production you'll want external managed services for Postgres, Redis, and NATS rather than the containerized versions. Set the corresponding `DATABASE_URL`, `REDIS_URL`, and `NATS_URL` environment variables. Put the frontend behind a CDN. Set `CORS_ORIGINS` to your actual domain. Make sure all secrets are real random values at least 32 characters long.

---

## Common issues

**Services failing to build with `missing go.sum entry`**

The Dockerfile needs to copy all source files before running `go mod tidy`, not before. Use this pattern:

```dockerfile
COPY . .
RUN GONOSUMDB=* GOFLAGS=-mod=mod go mod tidy
RUN CGO_ENABLED=0 GOOS=linux GONOSUMDB=* GOFLAGS=-mod=mod go build -o service .
```

**Backend fails to start**

Check postgres health first. The backend retries 10 times at 3-second intervals, but if postgres takes more than 30 seconds you'll see connection errors. `agentplane logs postgres` will show what's happening.

**pgvector extension missing**

The migrations enable it, but if you're connecting to external Postgres the instance needs pgvector installed. Most managed cloud providers support it as an optional extension.

**Control Brain shows no services**

Services self-register with the Control Brain on startup. If the brain started after the other services, they haven't had a chance to register yet. Either restart the services (`docker compose restart backend`) or wait for the next reconciliation cycle. The brain reconciles every 10 seconds by default.

**Memory search returns nothing**

Vector search requires `OPENAI_API_KEY` or a compatible embedding endpoint set in your `.env`. Without it, memories get stored but without embeddings, so similarity search has nothing to compare against.

---

## License

Enterprise. All rights reserved.