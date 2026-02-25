import time
import asyncio
import structlog
from contextlib import asynccontextmanager
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from prometheus_fastapi_instrumentator import Instrumentator
from database import engine, Base, AsyncSessionLocal
from services.redis_service import redis_service
from services.nats_service import nats_service
from routers.agents import router as agents_router
from routers.data import (
    logs_router, events_router, metrics_router,
    executions_router, alerts_router, containers_router,
)
from services.agent_service import agent_service
from sqlalchemy import update
import json
from config import settings

logger = structlog.get_logger()
START_TIME = time.time()


def setup_telemetry(app: FastAPI):
    try:
        provider = TracerProvider()
        otlp_exporter = OTLPSpanExporter(endpoint=settings.OTEL_EXPORTER_OTLP_ENDPOINT, insecure=True)
        provider.add_span_processor(BatchSpanProcessor(otlp_exporter))
        trace.set_tracer_provider(provider)
        FastAPIInstrumentor.instrument_app(app)
    except Exception as e:
        logger.warning("OpenTelemetry setup failed", error=str(e))


async def nats_event_handler(msg):
    try:
        data = json.loads(msg.data.decode())
        event_type = data.get("event_type", "")
        agent_id = data.get("agent_id")
        if not agent_id:
            return

        async with AsyncSessionLocal() as db:
            try:
                from models import Event, Agent
                import uuid
                from datetime import datetime, timezone

                # Only store non-duplicate events (control-plane already stores them)
                source = data.get("source", "")
                if source not in ("control-plane",):
                    event = Event(
                        agent_id=uuid.UUID(agent_id),
                        event_type=event_type,
                        payload=data,
                        source=data.get("source", "system"),
                        level=data.get("level", "info"),
                    )
                    db.add(event)

                status_map = {
                    "agent.started": "running",
                    "agent.stopped": "stopped",
                    "agent.failed": "error",
                    "agent.completed": "idle",
                }
                if event_type in status_map:
                    stmt = update(Agent).where(Agent.id == uuid.UUID(agent_id)).values(
                        status=status_map[event_type],
                        updated_at=datetime.now(timezone.utc),
                    )
                    await db.execute(stmt)
                    await redis_service.set_agent_state(agent_id, {"status": status_map[event_type]})

                    # Handle auto-restart on failure
                    if event_type == "agent.failed":
                        asyncio.create_task(agent_service.handle_agent_failure(db, agent_id))

                await db.commit()
            except Exception as e:
                await db.rollback()
                logger.error("Event handler error", error=str(e))
    except Exception as e:
        logger.error("NATS message parse error", error=str(e))


@asynccontextmanager
async def lifespan(app: FastAPI):
    logger.info("Starting AI Agent Infrastructure Platform")

    try:
        await redis_service.connect(settings.REDIS_URL)
        logger.info("Redis connected")
    except Exception as e:
        logger.warning("Redis connection failed", error=str(e))

    try:
        await nats_service.connect(settings.NATS_URL)
        await nats_service.subscribe("agents.events", nats_event_handler, queue="control-plane")
        logger.info("NATS connected and subscribed")
    except Exception as e:
        logger.warning("NATS connection failed", error=str(e))

    yield

    logger.info("Shutting down")
    await nats_service.disconnect()
    await redis_service.disconnect()
    await engine.dispose()


app = FastAPI(
    title="AI Agent Infrastructure Platform",
    version="2.0.0",
    lifespan=lifespan,
)
@app.on_event("startup")
async def startup():
    await redis_service.connect()
    await nats_service.connect()

@app.on_event("shutdown")
async def shutdown():
    await redis_service.disconnect()
    await nats_service.disconnect()
app.add_middleware(
    CORSMiddleware,
    allow_origins=["http://localhost:3000"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

setup_telemetry(app)
Instrumentator().instrument(app).expose(app)

app.include_router(agents_router, prefix="/api/v1")
app.include_router(logs_router, prefix="/api/v1")
app.include_router(events_router, prefix="/api/v1")
app.include_router(metrics_router, prefix="/api/v1")
app.include_router(executions_router, prefix="/api/v1")
app.include_router(alerts_router, prefix="/api/v1")
app.include_router(containers_router, prefix="/api/v1")


@app.get("/health")
async def health():
    return {"status": "healthy", "uptime": time.time() - START_TIME}


@app.get("/api/v1/system/health")
async def system_health():
    async with AsyncSessionLocal() as db:
        health = await agent_service.get_system_health(db)
        health["uptime_seconds"] = time.time() - START_TIME
        return health


if __name__ == "__main__":
    import uvicorn
    uvicorn.run("main:app", host="0.0.0.0", port=8000, reload=False, workers=1)