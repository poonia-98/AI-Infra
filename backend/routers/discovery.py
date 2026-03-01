import json
from datetime import datetime, timezone
from typing import Any, Optional

from fastapi import APIRouter, Depends, Query
from pydantic import BaseModel, Field
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from database import get_db


router = APIRouter(prefix="/api/v1/discovery", tags=["discovery"])


class RegistryUpsert(BaseModel):
    agent_id: str
    organisation_id: Optional[str] = None
    endpoint: str
    protocol: str = "nats"
    capabilities: list[str] = Field(default_factory=list)
    health_status: str = "healthy"
    metadata: dict[str, Any] = Field(default_factory=dict)


@router.post("/register", status_code=201)
async def register(payload: RegistryUpsert, db: AsyncSession = Depends(get_db)):
    capabilities = "{" + ",".join(payload.capabilities) + "}"
    result = await db.execute(
        text(
            """
            INSERT INTO agent_service_registry
              (agent_id, organisation_id, endpoint, protocol, capabilities, health_status, metadata, last_heartbeat)
            VALUES
              (:agent_id::uuid, :organisation_id::uuid, :endpoint, :protocol, :capabilities::text[],
               :health_status, :metadata::jsonb, NOW())
            ON CONFLICT (agent_id) DO UPDATE SET
              organisation_id=EXCLUDED.organisation_id,
              endpoint=EXCLUDED.endpoint,
              protocol=EXCLUDED.protocol,
              capabilities=EXCLUDED.capabilities,
              health_status=EXCLUDED.health_status,
              metadata=EXCLUDED.metadata,
              last_heartbeat=NOW(),
              updated_at=NOW()
            RETURNING id::text
            """
        ),
        {
            "agent_id": payload.agent_id,
            "organisation_id": payload.organisation_id,
            "endpoint": payload.endpoint,
            "protocol": payload.protocol,
            "capabilities": capabilities,
            "health_status": payload.health_status,
            "metadata": json.dumps(payload.metadata),
        },
    )
    row = result.fetchone()
    return {"id": row[0] if row else ""}


@router.post("/heartbeat/{agent_id}")
async def heartbeat(agent_id: str, health_status: str = "healthy", db: AsyncSession = Depends(get_db)):
    await db.execute(
        text(
            """
            UPDATE agent_service_registry
            SET health_status=:health_status, last_heartbeat=NOW(), updated_at=NOW()
            WHERE agent_id=:agent_id::uuid
            """
        ),
        {"agent_id": agent_id, "health_status": health_status},
    )
    return {"status": "ok", "agent_id": agent_id, "timestamp": datetime.now(timezone.utc)}


@router.get("/agents")
async def discover(
    organisation_id: Optional[str] = None,
    capability: Optional[str] = None,
    health_status: str = "healthy",
    limit: int = Query(200, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
):
    clauses = ["health_status=:health_status"]
    params: dict[str, Any] = {"health_status": health_status, "limit": limit}
    if organisation_id:
        clauses.append("organisation_id=:organisation_id::uuid")
        params["organisation_id"] = organisation_id
    if capability:
        clauses.append(":capability = ANY(capabilities)")
        params["capability"] = capability
    where = " AND ".join(clauses)
    rows = await db.execute(
        text(
            f"""
            SELECT id::text, agent_id::text, organisation_id::text, endpoint, protocol,
                   capabilities, health_status, last_heartbeat, metadata, created_at, updated_at
            FROM agent_service_registry
            WHERE {where}
            ORDER BY last_heartbeat DESC
            LIMIT :limit
            """
        ),
        params,
    )
    return [dict(r._mapping) for r in rows.fetchall()]
