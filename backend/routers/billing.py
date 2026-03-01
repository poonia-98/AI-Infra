import json
from typing import Any, Optional

from fastapi import APIRouter, Depends, Query
from pydantic import BaseModel, Field
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from database import get_db


router = APIRouter(prefix="/api/v1/billing", tags=["billing"])


class BillingEventCreate(BaseModel):
    organisation_id: Optional[str] = None
    agent_id: Optional[str] = None
    workflow_id: Optional[str] = None
    execution_id: Optional[str] = None
    event_type: str
    quantity: float = 0
    unit: str = "unit"
    cost_usd: float = 0
    metadata: dict[str, Any] = Field(default_factory=dict)


@router.post("/events", status_code=201)
async def create_event(payload: BillingEventCreate, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text(
            """
            INSERT INTO billing_events
              (organisation_id, agent_id, workflow_id, execution_id, event_type, quantity, unit, cost_usd, metadata)
            VALUES
              (:organisation_id::uuid, :agent_id::uuid, :workflow_id::uuid, :execution_id::uuid,
               :event_type, :quantity, :unit, :cost_usd, :metadata::jsonb)
            RETURNING id::text
            """
        ),
        {
            "organisation_id": payload.organisation_id,
            "agent_id": payload.agent_id,
            "workflow_id": payload.workflow_id,
            "execution_id": payload.execution_id,
            "event_type": payload.event_type,
            "quantity": payload.quantity,
            "unit": payload.unit,
            "cost_usd": payload.cost_usd,
            "metadata": json.dumps(payload.metadata),
        },
    )
    row = result.fetchone()
    return {"id": row[0] if row else ""}


@router.get("/events")
async def list_events(
    organisation_id: Optional[str] = None,
    agent_id: Optional[str] = None,
    event_type: Optional[str] = None,
    limit: int = Query(200, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
):
    clauses = ["1=1"]
    params: dict[str, Any] = {"limit": limit}
    if organisation_id:
        clauses.append("organisation_id=:organisation_id::uuid")
        params["organisation_id"] = organisation_id
    if agent_id:
        clauses.append("agent_id=:agent_id::uuid")
        params["agent_id"] = agent_id
    if event_type:
        clauses.append("event_type=:event_type")
        params["event_type"] = event_type
    where = " AND ".join(clauses)
    rows = await db.execute(
        text(
            f"""
            SELECT id::text, organisation_id::text, agent_id::text, workflow_id::text,
                   execution_id::text, event_type, quantity, unit, cost_usd,
                   currency, metadata, created_at
            FROM billing_events
            WHERE {where}
            ORDER BY created_at DESC
            LIMIT :limit
            """
        ),
        params,
    )
    return [dict(r._mapping) for r in rows.fetchall()]


@router.get("/summary")
async def summary(
    organisation_id: str,
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(
        text(
            """
            SELECT event_type, SUM(quantity) AS total_quantity, SUM(cost_usd) AS total_cost
            FROM billing_events
            WHERE organisation_id=:organisation_id::uuid
            GROUP BY event_type
            ORDER BY total_cost DESC
            """
        ),
        {"organisation_id": organisation_id},
    )
    return [dict(r._mapping) for r in rows.fetchall()]
