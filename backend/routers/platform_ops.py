"""
API routers for:
  - /api/v1/configs          — config management
  - /api/v1/secrets          — secrets management (names only, never values in responses)
  - /api/v1/audit            — audit log queries
  - /api/v1/execution-queue  — execution queue status
"""
import uuid
import httpx
from typing import Optional
from datetime import datetime

from fastapi import APIRouter, Depends, HTTPException, Query
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from pydantic import BaseModel, Field

from database import get_db
from rbac import require_permission, Permission
from services.config_service import ConfigService, SecretsService
from config import settings

# ── Config Router ─────────────────────────────────────────────────────────────

configs_router = APIRouter(prefix="/api/v1/configs", tags=["configs"])


class ConfigUpsert(BaseModel):
    service: str
    key: str
    value: str
    value_type: str = Field("string", pattern="^(string|int|bool|json)$")
    description: Optional[str] = None


class ConfigResponse(BaseModel):
    id: str
    service: str
    key: str
    value: str
    value_type: str
    description: Optional[str]
    version: int
    updated_at: datetime


@configs_router.get("", response_model=list[ConfigResponse],
                    dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def list_configs(
    service: Optional[str] = None,
    db: AsyncSession = Depends(get_db),
):
    q = "SELECT id::text, service, key, value, value_type, description, version, updated_at FROM configs"
    params = {}
    if service:
        q += " WHERE service=:s"
        params["s"] = service
    q += " ORDER BY service, key"
    rows = await db.execute(text(q), params)
    return [
        ConfigResponse(id=r[0], service=r[1], key=r[2], value=r[3],
                       value_type=r[4], description=r[5], version=r[6], updated_at=r[7])
        for r in rows.fetchall()
    ]


@configs_router.get("/{service}/{key}")
async def get_config(service: str, key: str, db: AsyncSession = Depends(get_db)):
    value = await ConfigService.get(db, service, key)
    if value is None:
        raise HTTPException(status_code=404, detail="Config not found")
    return {"service": service, "key": key, "value": value}


@configs_router.put("",
                    dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def upsert_config(body: ConfigUpsert, db: AsyncSession = Depends(get_db)):
    await ConfigService.set(
        db, body.service, body.key, body.value,
        value_type=body.value_type, description=body.description,
    )
    return {"service": body.service, "key": body.key, "status": "ok"}


@configs_router.delete("/{service}/{key}", status_code=204,
                       dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def delete_config(service: str, key: str, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text("DELETE FROM configs WHERE service=:s AND key=:k RETURNING id"),
        {"s": service, "k": key},
    )
    if result.rowcount == 0:
        raise HTTPException(status_code=404, detail="Config not found")
    ConfigService.invalidate(service, key)


@configs_router.get("/{service}/{key}/history",
                    dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def config_history(service: str, key: str, limit: int = 20,
                         db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT ch.old_value, ch.new_value, ch.version, ch.changed_by, ch.changed_at
        FROM config_history ch
        JOIN configs c ON ch.config_id = c.id
        WHERE c.service=:s AND c.key=:k
        ORDER BY ch.changed_at DESC LIMIT :l
    """), {"s": service, "k": key, "l": limit})
    return [
        {"old_value": r[0], "new_value": r[1], "version": r[2],
         "changed_by": r[3], "changed_at": r[4]}
        for r in rows.fetchall()
    ]


# ── Secrets Router ────────────────────────────────────────────────────────────

secrets_router = APIRouter(prefix="/api/v1/secrets", tags=["secrets"])


class SecretCreate(BaseModel):
    service: str
    name: str
    value: str
    description: Optional[str] = None
    expires_at: Optional[datetime] = None


@secrets_router.get("/{service}",
                    dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def list_secret_names(service: str, db: AsyncSession = Depends(get_db)):
    """List secret names only — values are NEVER returned via API."""
    names = await SecretsService.list_names(db, service)
    return {"service": service, "secrets": names}


@secrets_router.post("", status_code=201,
                     dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def create_secret(body: SecretCreate, db: AsyncSession = Depends(get_db)):
    await SecretsService.set(
        db, body.service, body.name, body.value,
        description=body.description, expires_at=body.expires_at,
    )
    return {"service": body.service, "name": body.name, "status": "created"}


@secrets_router.delete("/{service}/{name}", status_code=204,
                       dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def delete_secret(service: str, name: str, db: AsyncSession = Depends(get_db)):
    deleted = await SecretsService.delete(db, service, name)
    if not deleted:
        raise HTTPException(status_code=404, detail="Secret not found")


# ── Audit Log Router ──────────────────────────────────────────────────────────

audit_router = APIRouter(prefix="/api/v1/audit", tags=["audit"])


@audit_router.get("",
                  dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def list_audit_logs(
    actor_id: Optional[str] = None,
    action: Optional[str] = None,
    resource: Optional[str] = None,
    resource_id: Optional[str] = None,
    service: Optional[str] = None,
    since: Optional[str] = None,
    limit: int = Query(50, ge=1, le=500),
    offset: int = Query(0, ge=0),
    db: AsyncSession = Depends(get_db),
):
    conditions = ["1=1"]
    params: dict = {"limit": limit, "offset": offset}

    if actor_id:
        conditions.append("actor_id=:actor_id")
        params["actor_id"] = actor_id
    if action:
        conditions.append("action LIKE :action")
        params["action"] = f"%{action}%"
    if resource:
        conditions.append("resource=:resource")
        params["resource"] = resource
    if resource_id:
        try:
            uuid.UUID(resource_id)
            conditions.append("resource_id=:resource_id::uuid")
            params["resource_id"] = resource_id
        except ValueError:
            pass
    if service:
        conditions.append("service=:service")
        params["service"] = service
    if since:
        try:
            datetime.fromisoformat(since)
            conditions.append("created_at >= :since")
            params["since"] = since
        except ValueError:
            pass

    where = " AND ".join(conditions)
    rows = await db.execute(text(f"""
        SELECT id::text, actor_id, actor_type, actor_ip::text,
               action, resource, resource_id::text,
               new_value, metadata, request_id, service, status, created_at
        FROM audit_logs
        WHERE {where}
        ORDER BY created_at DESC
        LIMIT :limit OFFSET :offset
    """), params)
    return [
        {
            "id": r[0], "actor_id": r[1], "actor_type": r[2], "actor_ip": r[3],
            "action": r[4], "resource": r[5], "resource_id": r[6],
            "new_value": r[7], "metadata": r[8], "request_id": r[9],
            "service": r[10], "status": r[11], "created_at": r[12],
        }
        for r in rows.fetchall()
    ]


# ── Execution Queue Router ────────────────────────────────────────────────────

queue_router = APIRouter(
    prefix="/api/v1/execution-queue",
    tags=["execution-queue"],
    dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))],
)


@queue_router.get("")
async def list_queue(
    status: Optional[str] = None,
    node: Optional[str] = None,
    limit: int = Query(50, ge=1, le=200),
    db: AsyncSession = Depends(get_db),
):
    conditions = ["1=1"]
    params: dict = {"limit": limit}
    if status:
        conditions.append("eq.status=:status")
        params["status"] = status
    if node:
        conditions.append("eq.assigned_node=:node")
        params["node"] = node

    where = " AND ".join(conditions)
    rows = await db.execute(text(f"""
        SELECT eq.id::text, eq.execution_id::text, eq.agent_id::text,
               eq.status, eq.assigned_node, eq.priority,
               eq.locked_by, eq.lock_expires,
               eq.scheduled_at, eq.started_at, eq.completed_at,
               eq.attempt, eq.max_attempts, eq.last_error
        FROM execution_queue eq
        WHERE {where}
        ORDER BY eq.priority ASC, eq.scheduled_at ASC
        LIMIT :limit
    """), params)

    return [
        {
            "id": r[0], "execution_id": r[1], "agent_id": r[2],
            "status": r[3], "assigned_node": r[4], "priority": r[5],
            "locked_by": r[6], "lock_expires": r[7],
            "scheduled_at": r[8], "started_at": r[9], "completed_at": r[10],
            "attempt": r[11], "max_attempts": r[12], "last_error": r[13],
        }
        for r in rows.fetchall()
    ]


@queue_router.delete("/{queue_id}", status_code=204,
                     dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def cancel_queue_entry(queue_id: uuid.UUID, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text("UPDATE execution_queue SET status='cancelled' WHERE id=:id AND status='pending' RETURNING id"),
        {"id": str(queue_id)},
    )
    if result.rowcount == 0:
        raise HTTPException(status_code=404, detail="Queue entry not found or not cancellable")


class QueueEnqueueRequest(BaseModel):
    organisation_id: Optional[str] = None
    agent_id: str
    workflow_exec_id: Optional[str] = None
    queue_type: str = "standard"
    priority: int = 5
    max_attempts: int = 3
    dedup_key: Optional[str] = None
    input: dict = Field(default_factory=dict)
    visibility_timeout_seconds: int = 120


class QueueClaimRequest(BaseModel):
    worker_id: str
    limit: int = 20


class QueueWorkerRequest(BaseModel):
    worker_id: str


class QueueFailRequest(BaseModel):
    worker_id: str
    error: str
    retry_delay_seconds: int = 0


@queue_router.post("/enqueue")
async def queue_enqueue(body: QueueEnqueueRequest):
    async with httpx.AsyncClient(timeout=15.0) as client:
        resp = await client.post(
            f"{settings.EXECUTION_QUEUE_URL}/queue/enqueue",
            json=body.model_dump(),
            headers={"X-Service-API-Key": settings.CONTROL_PLANE_API_KEY},
        )
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail="Execution queue request failed")
    return resp.json()


@queue_router.post("/claim")
async def queue_claim(body: QueueClaimRequest):
    async with httpx.AsyncClient(timeout=15.0) as client:
        resp = await client.post(
            f"{settings.EXECUTION_QUEUE_URL}/queue/claim",
            json=body.model_dump(),
            headers={"X-Service-API-Key": settings.CONTROL_PLANE_API_KEY},
        )
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail="Execution queue request failed")
    return resp.json()


@queue_router.post("/{queue_id}/ack")
async def queue_ack(queue_id: str, body: QueueWorkerRequest):
    async with httpx.AsyncClient(timeout=15.0) as client:
        resp = await client.post(
            f"{settings.EXECUTION_QUEUE_URL}/queue/{queue_id}/ack",
            json=body.model_dump(),
            headers={"X-Service-API-Key": settings.CONTROL_PLANE_API_KEY},
        )
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail="Execution queue request failed")
    return resp.json()


@queue_router.post("/{queue_id}/fail")
async def queue_fail(queue_id: str, body: QueueFailRequest):
    async with httpx.AsyncClient(timeout=15.0) as client:
        resp = await client.post(
            f"{settings.EXECUTION_QUEUE_URL}/queue/{queue_id}/fail",
            json=body.model_dump(),
            headers={"X-Service-API-Key": settings.CONTROL_PLANE_API_KEY},
        )
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail="Execution queue request failed")
    return resp.json()


@queue_router.post("/{queue_id}/heartbeat")
async def queue_heartbeat(queue_id: str, body: QueueWorkerRequest):
    async with httpx.AsyncClient(timeout=15.0) as client:
        resp = await client.post(
            f"{settings.EXECUTION_QUEUE_URL}/queue/{queue_id}/heartbeat",
            json=body.model_dump(),
            headers={"X-Service-API-Key": settings.CONTROL_PLANE_API_KEY},
        )
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail="Execution queue request failed")
    return resp.json()


@queue_router.get("/stats")
async def queue_stats_proxy():
    async with httpx.AsyncClient(timeout=15.0) as client:
        resp = await client.get(
            f"{settings.EXECUTION_QUEUE_URL}/queue/stats",
            headers={"X-Service-API-Key": settings.CONTROL_PLANE_API_KEY},
        )
    if resp.status_code >= 400:
        raise HTTPException(status_code=502, detail="Execution queue request failed")
    return resp.json()


