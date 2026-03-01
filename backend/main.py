"""
AgentPlane — AI Agent Operating System
Backend API  v3.0.0

FastAPI application wiring all routers, middleware, and service integrations.
Event-driven via NATS JetStream. State cached in Redis. Observability via OTEL.
"""
from __future__ import annotations

import asyncio
import json
import time
from contextlib import asynccontextmanager

import structlog
from fastapi import FastAPI, Depends
from fastapi.middleware.cors import CORSMiddleware
from starlette.middleware.base import BaseHTTPMiddleware
from prometheus_fastapi_instrumentator import Instrumentator
from sqlalchemy import update

# ── OTEL ──────────────────────────────────────────────────────────────────────
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

# ── Database ──────────────────────────────────────────────────────────────────
from database import engine, Base, AsyncSessionLocal

# ── Core services ──────────────────────────────────────────────────────────────
from services.redis_service import redis_service
from services.nats_service import nats_service
from services.agent_service import agent_service
from services.brain_service import brain_service
from services.cache_service import init_cache, get_cache
from services.task_registry import task_registry

# ── Middleware ────────────────────────────────────────────────────────────────
from middleware.audit import AuditMiddleware
from middleware.rate_limit import RateLimitMiddleware, init_limiter

# ── Config ────────────────────────────────────────────────────────────────────
from config import settings

# ── Auth dependency (used on protected routes) ────────────────────────────────
from routers.enterprise.auth import get_current_user
protected = []  # Add [Depends(get_current_user)] to enforce auth globally

logger = structlog.get_logger()
START_TIME = time.time()


# ── Telemetry ─────────────────────────────────────────────────────────────────

def setup_telemetry(app: FastAPI) -> None:
    try:
        provider = TracerProvider()
        exporter = OTLPSpanExporter(
            endpoint=settings.OTEL_EXPORTER_OTLP_ENDPOINT,
            insecure=True,
        )
        provider.add_span_processor(BatchSpanProcessor(exporter))
        trace.set_tracer_provider(provider)
        FastAPIInstrumentor.instrument_app(app)
        logger.info("OpenTelemetry configured")
    except Exception as e:
        logger.warning("OpenTelemetry setup failed", error=str(e))


# ── NATS Event Handler ────────────────────────────────────────────────────────

async def nats_event_handler(msg) -> None:
    """
    Central NATS subscriber for agent lifecycle events.
    Updates Redis state cache without hitting the DB for every event.
    """
    try:
        data = json.loads(msg.data.decode())
    except Exception:
        return

    event_type = data.get("event_type", "")
    agent_id   = data.get("agent_id")
    if not agent_id:
        return

    status_map = {
        "agent.started":   "running",
        "agent.stopped":   "stopped",
        "agent.failed":    "error",
        "agent.completed": "idle",
        "AgentStarted":    "running",
        "AgentStopped":    "stopped",
        "AgentFailed":     "error",
    }

    async with AsyncSessionLocal() as db:
        try:
            from models import Event, Agent
            import uuid
            from datetime import datetime, timezone

            # Write event record (skip self-generated events)
            if data.get("source") not in ("control-plane", "backend"):
                evt = Event(
                    agent_id=uuid.UUID(agent_id),
                    event_type=event_type,
                    payload=data,
                    source=data.get("source", "system"),
                    level=data.get("level", "info"),
                )
                db.add(evt)

            # Update agent status in DB + Redis cache
            new_status = status_map.get(event_type)
            if new_status:
                await db.execute(
                    update(Agent)
                    .where(Agent.id == uuid.UUID(agent_id))
                    .values(status=new_status, updated_at=datetime.now(timezone.utc))
                )
                # Update Redis cache — no polling needed
                await redis_service.set_agent_state(agent_id, {"status": new_status})
                # Push to cache service ring buffer
                cache = get_cache()
                if cache:
                    await cache.push_event(
                        data.get("organisation_id", "global"),
                        {**data, "timestamp": datetime.now(timezone.utc).isoformat()},
                    )

                if event_type in ("agent.failed", "AgentFailed"):
                    asyncio.create_task(
                        agent_service.handle_agent_failure(db, agent_id)
                    )

            await db.commit()

        except Exception as e:
            await db.rollback()
            logger.error("Event handler error", event=event_type, agent=agent_id, error=str(e))


# ── Rate limit helper ─────────────────────────────────────────────────────────

async def _get_org_rpm(org_id: str) -> int:
    try:
        async with AsyncSessionLocal() as db:
            from sqlalchemy import text
            row = await db.execute(
                text("SELECT max_api_rpm FROM organisation_quotas WHERE organisation_id=:id::uuid"),
                {"id": org_id},
            )
            rec = row.fetchone()
            if rec:
                return rec[0]
    except Exception:
        pass
    return settings.RATE_LIMIT_ORG_RPM


# ── Lifespan ──────────────────────────────────────────────────────────────────

@asynccontextmanager
async def lifespan(app: FastAPI):
    logger.info("AgentPlane starting", version="3.0.0")

    # ── DB tables ──────────────────────────────────────────────────────────────
    for attempt in range(10):
        try:
            async with engine.begin() as conn:
                await conn.run_sync(Base.metadata.create_all)
            logger.info("Database tables ready")
            break
        except Exception as e:
            logger.warning("DB not ready", attempt=attempt + 1, error=str(e))
            await asyncio.sleep(3)

    # ── Redis + cache ──────────────────────────────────────────────────────────
    try:
        await redis_service.connect(settings.REDIS_URL)
        init_cache(redis_service)
        if settings.RATE_LIMIT_ENABLED:
            init_limiter(redis_service.client)
        logger.info("Redis connected and cache initialized")
    except Exception as e:
        logger.warning("Redis connection failed", error=str(e))

    # ── NATS ──────────────────────────────────────────────────────────────────
    try:
        await nats_service.connect(settings.NATS_URL)
        await nats_service.subscribe(
            "agents.events", nats_event_handler, queue="control-plane"
        )
        logger.info("NATS connected — subscribed to agents.events")
    except Exception as e:
        logger.warning("NATS connection failed", error=str(e))

    # ── Register with Control Brain ────────────────────────────────────────────
    try:
        await brain_service.register_service(
            service_name="backend",
            service_url="http://backend:8000",
            health_endpoint="http://backend:8000/health",
            capabilities=["agents", "executions", "billing", "auth", "memory", "federation", "time-travel"],
            version="3.0.0",
        )
        logger.info("Registered with control-brain")
    except Exception as e:
        logger.warning("Brain registration skipped", error=str(e))

    yield

    # ── Shutdown ───────────────────────────────────────────────────────────────
    logger.info("AgentPlane shutting down")
    try:
        await task_registry.shutdown()
    except Exception:
        pass
    await nats_service.disconnect()
    await redis_service.disconnect()
    await engine.dispose()
    await brain_service.close()


# ── FastAPI App ────────────────────────────────────────────────────────────────

app = FastAPI(
    title="AgentPlane — AI Agent Operating System",
    version="3.0.0",
    description="Enterprise AI Agent Control Plane",
    docs_url="/docs",
    redoc_url="/redoc",
    lifespan=lifespan,
)

# ── CORS ──────────────────────────────────────────────────────────────────────
app.add_middleware(
    CORSMiddleware,
    allow_origins=getattr(settings, "CORS_ORIGINS_LIST", ["*"]),
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

# ── Security headers ───────────────────────────────────────────────────────────
class SecurityHeadersMiddleware(BaseHTTPMiddleware):
    async def dispatch(self, request, call_next):
        response = await call_next(request)
        response.headers["X-Content-Type-Options"] = "nosniff"
        response.headers["X-Frame-Options"]         = "DENY"
        response.headers["X-XSS-Protection"]        = "1; mode=block"
        response.headers["Referrer-Policy"]          = "strict-origin-when-cross-origin"
        response.headers["Permissions-Policy"]       = "geolocation=(), camera=()"
        return response

app.add_middleware(SecurityHeadersMiddleware)
app.add_middleware(AuditMiddleware)

if settings.RATE_LIMIT_ENABLED:
    app.add_middleware(RateLimitMiddleware, org_quota_fn=_get_org_rpm)

setup_telemetry(app)
Instrumentator().instrument(app).expose(app)


# ── Routers ───────────────────────────────────────────────────────────────────

# Core data plane
from routers.agents import router as agents_router
from routers.data import (
    logs_router, events_router, metrics_router,
    executions_router, alerts_router, containers_router,
)
from routers.infrastructure import schedules_router, nodes_router
from routers.platform_ops import configs_router, secrets_router, audit_router, queue_router

# Enterprise: auth, orgs, platform features
from routers.enterprise.auth import auth_router
from routers.enterprise.organisations import orgs_router
from routers.enterprise.platform import (
    workflows_router, versions_router, webhooks_router,
    memory_router, messages_router, autoscale_router, marketplace_router,
)

# Cloud platform extension (v3.0)
from routers.cloud import (
    billing_router,
    observability_router,
    sandbox_router,
    simulation_router,
    regions_router,
    federation_router,
    evolution_router,
    knowledge_router,
    vault_router,
)

# Advanced features
from routers.time_travel import router as time_travel_router
from routers.brain import router as brain_router

# ── Register all routers ──────────────────────────────────────────────────────

# Core
app.include_router(agents_router,     prefix="/api/v1")
app.include_router(logs_router,       prefix="/api/v1")
app.include_router(events_router,     prefix="/api/v1")
app.include_router(metrics_router,    prefix="/api/v1")
app.include_router(executions_router, prefix="/api/v1")
app.include_router(alerts_router,     prefix="/api/v1")
app.include_router(containers_router, prefix="/api/v1")
app.include_router(schedules_router)
app.include_router(nodes_router)
app.include_router(configs_router)
app.include_router(secrets_router)
app.include_router(audit_router)
app.include_router(queue_router)

# Enterprise
app.include_router(auth_router)
app.include_router(orgs_router)
app.include_router(workflows_router)
app.include_router(versions_router)
app.include_router(webhooks_router)
app.include_router(memory_router)
app.include_router(messages_router)
app.include_router(autoscale_router)
app.include_router(marketplace_router)

# Cloud Platform (v3.0)
app.include_router(billing_router)
app.include_router(observability_router)
app.include_router(sandbox_router)
app.include_router(simulation_router)
app.include_router(regions_router)
app.include_router(federation_router)
app.include_router(evolution_router)
app.include_router(knowledge_router)
app.include_router(vault_router)

# Advanced features
app.include_router(time_travel_router)
app.include_router(brain_router, dependencies=protected)


# ── Platform endpoints ────────────────────────────────────────────────────────

@app.get("/health", tags=["platform"])
async def health():
    """Liveness probe."""
    return {
        "status":  "healthy",
        "service": "backend",
        "version": "3.0.0",
        "uptime":  round(time.time() - START_TIME, 1),
    }


@app.get("/ready", tags=["platform"])
async def readiness():
    """Readiness probe — checks DB connectivity."""
    from sqlalchemy import text
    from fastapi import HTTPException
    try:
        async with AsyncSessionLocal() as db:
            await db.execute(text("SELECT 1"))
        return {"ready": True}
    except Exception as e:
        raise HTTPException(503, detail=f"Database not ready: {e}")


@app.get("/api/v1/system/health", tags=["platform"])
async def system_health():
    """Full system health check including all connected services."""
    async with AsyncSessionLocal() as db:
        h = await agent_service.get_system_health(db)
    h["uptime_seconds"] = round(time.time() - START_TIME, 1)
    h["version"] = "3.0.0"
    h["features"] = {
        "kubernetes":     settings.K8S_ENABLED,
        "rate_limiting":  settings.RATE_LIMIT_ENABLED,
        "sso_google":     bool(settings.GOOGLE_CLIENT_ID),
        "sso_github":     bool(settings.GITHUB_CLIENT_ID),
        "vector_memory":  bool(getattr(settings, "VECTOR_URL", "")),
        "control_brain":  bool(getattr(settings, "CONTROL_BRAIN_URL", "")),
        "federation":     bool(getattr(settings, "AGENT_FEDERATION_URL", "")),
    }
    # Brain health
    h["brain_online"] = await brain_service.health()
    return h


if __name__ == "__main__":
    import uvicorn
    uvicorn.run("main:app", host="0.0.0.0", port=8000, reload=False, workers=1)