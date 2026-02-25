import time
from typing import Any
from fastapi import APIRouter
from sqlalchemy import text
from database import AsyncSessionLocal
from services.redis_service import redis_service
from services.nats_service import nats_service
import httpx
from config import settings

router = APIRouter(tags=["health"])

START_TIME = time.time()


async def check_postgres() -> dict[str, Any]:
    try:
        async with AsyncSessionLocal() as session:
            await session.execute(text("SELECT 1"))
        return {"status": "healthy", "latency_ms": None}
    except Exception as e:
        return {"status": "unhealthy", "error": str(e)}


async def check_redis() -> dict[str, Any]:
    try:
        start = time.monotonic()
        await redis_service.client.ping()
        latency = round((time.monotonic() - start) * 1000, 2)
        return {"status": "healthy", "latency_ms": latency}
    except Exception as e:
        return {"status": "unhealthy", "error": str(e)}


async def check_nats() -> dict[str, Any]:
    try:
        if nats_service.nc and not nats_service.nc.is_closed:
            return {"status": "healthy", "connected_url": str(settings.NATS_URL)}
        return {"status": "unhealthy", "error": "Not connected"}
    except Exception as e:
        return {"status": "unhealthy", "error": str(e)}


async def check_executor() -> dict[str, Any]:
    try:
        async with httpx.AsyncClient(timeout=3.0) as client:
            r = await client.get(f"{settings.EXECUTOR_URL}/health")
            return {"status": "healthy" if r.status_code == 200 else "degraded"}
    except Exception as e:
        return {"status": "unhealthy", "error": str(e)}


async def check_ws_gateway() -> dict[str, Any]:
    try:
        async with httpx.AsyncClient(timeout=3.0) as client:
            r = await client.get(f"{settings.WS_GATEWAY_URL}/health")
            data = r.json()
            return {"status": "healthy", "connections": data.get("connections", 0)}
    except Exception as e:
        return {"status": "unhealthy", "error": str(e)}


@router.get("/health")
async def health_simple():
    return {"status": "healthy", "uptime_seconds": round(time.time() - START_TIME, 2)}


@router.get("/health/detailed")
async def health_detailed():
    postgres = await check_postgres()
    redis = await check_redis()
    nats = await check_nats()
    executor = await check_executor()
    ws = await check_ws_gateway()

    checks = {
        "postgres": postgres,
        "redis": redis,
        "nats": nats,
        "executor": executor,
        "ws_gateway": ws,
    }

    all_healthy = all(c["status"] == "healthy" for c in checks.values())
    any_unhealthy = any(c["status"] == "unhealthy" for c in checks.values())

    overall = "healthy" if all_healthy else ("degraded" if not any_unhealthy else "unhealthy")

    return {
        "status": overall,
        "uptime_seconds": round(time.time() - START_TIME, 2),
        "checks": checks,
    }


@router.get("/ready")
async def readiness():
    postgres = await check_postgres()
    if postgres["status"] != "healthy":
        from fastapi import HTTPException
        raise HTTPException(status_code=503, detail="Database not ready")
    return {"ready": True}


@router.get("/live")
async def liveness():
    return {"alive": True}