import uuid
import httpx
from datetime import datetime, timezone
from typing import Optional
from fastapi import APIRouter, Depends, HTTPException, Query
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import select, update, delete, desc
from pydantic import BaseModel, Field

from database import get_db
from models import AgentSchedule, ExecutorNode, Agent
from rbac import require_permission, Permission

# ── Schedules router ──────────────────────────────────────────────────────────

schedules_router = APIRouter(prefix="/api/v1/schedules", tags=["schedules"])
nodes_router = APIRouter(prefix="/api/v1/nodes", tags=["nodes"])


class ScheduleCreate(BaseModel):
    agent_id: uuid.UUID
    schedule_type: str = Field(..., pattern="^(cron|once|interval)$")
    cron_expr: Optional[str] = None
    interval_seconds: Optional[int] = Field(None, ge=1)
    run_at: Optional[datetime] = None
    timezone: str = "UTC"
    input: dict = {}
    max_runs: Optional[int] = Field(None, ge=1)
    enabled: bool = True


class ScheduleResponse(BaseModel):
    id: str
    agent_id: str
    schedule_type: str
    cron_expr: Optional[str]
    interval_seconds: Optional[int]
    run_at: Optional[datetime]
    enabled: bool
    timezone: str
    last_run_at: Optional[datetime]
    next_run_at: Optional[datetime]
    run_count: int
    max_runs: Optional[int]
    created_at: datetime

    model_config = {"from_attributes": True}

    @classmethod
    def from_orm(cls, obj: AgentSchedule) -> "ScheduleResponse":
        return cls(
            id=str(obj.id),
            agent_id=str(obj.agent_id),
            schedule_type=obj.schedule_type,
            cron_expr=obj.cron_expr,
            interval_seconds=obj.interval_seconds,
            run_at=obj.run_at,
            enabled=obj.enabled,
            timezone=obj.timezone,
            last_run_at=obj.last_run_at,
            next_run_at=obj.next_run_at,
            run_count=obj.run_count,
            max_runs=obj.max_runs,
            created_at=obj.created_at,
        )


@schedules_router.get("", response_model=list[ScheduleResponse])
async def list_schedules(
    agent_id: Optional[str] = None,
    enabled: Optional[bool] = None,
    db: AsyncSession = Depends(get_db),
):
    q = select(AgentSchedule).order_by(desc(AgentSchedule.created_at))
    if agent_id:
        try:
            q = q.where(AgentSchedule.agent_id == uuid.UUID(agent_id))
        except ValueError:
            pass
    if enabled is not None:
        q = q.where(AgentSchedule.enabled == enabled)
    result = await db.execute(q)
    return [ScheduleResponse.from_orm(s) for s in result.scalars().all()]


@schedules_router.post("", response_model=ScheduleResponse, status_code=201,
                       dependencies=[Depends(require_permission(Permission.MANAGE_SCHEDULES))])
async def create_schedule(body: ScheduleCreate, db: AsyncSession = Depends(get_db)):
    # Validate agent exists
    agent = await db.get(Agent, body.agent_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")

    # Validate schedule type parameters
    if body.schedule_type == "cron" and not body.cron_expr:
        raise HTTPException(status_code=400, detail="cron_expr required for cron schedule")
    if body.schedule_type == "interval" and not body.interval_seconds:
        raise HTTPException(status_code=400, detail="interval_seconds required for interval schedule")
    if body.schedule_type == "once" and not body.run_at:
        raise HTTPException(status_code=400, detail="run_at required for once schedule")

    sched = AgentSchedule(
        agent_id=body.agent_id,
        schedule_type=body.schedule_type,
        cron_expr=body.cron_expr,
        interval_seconds=body.interval_seconds,
        run_at=body.run_at,
        timezone=body.timezone,
        input=body.input,
        max_runs=body.max_runs,
        enabled=body.enabled,
    )
    # Set next_run_at for once schedules
    if body.schedule_type == "once" and body.run_at:
        sched.next_run_at = body.run_at

    db.add(sched)
    await db.flush()
    return ScheduleResponse.from_orm(sched)


@schedules_router.get("/{schedule_id}", response_model=ScheduleResponse)
async def get_schedule(schedule_id: uuid.UUID, db: AsyncSession = Depends(get_db)):
    sched = await db.get(AgentSchedule, schedule_id)
    if not sched:
        raise HTTPException(status_code=404, detail="Schedule not found")
    return ScheduleResponse.from_orm(sched)


@schedules_router.patch("/{schedule_id}", response_model=ScheduleResponse,
                        dependencies=[Depends(require_permission(Permission.MANAGE_SCHEDULES))])
async def update_schedule(schedule_id: uuid.UUID, body: dict, db: AsyncSession = Depends(get_db)):
    sched = await db.get(AgentSchedule, schedule_id)
    if not sched:
        raise HTTPException(status_code=404, detail="Schedule not found")
    allowed = {"enabled", "cron_expr", "interval_seconds", "run_at", "timezone", "max_runs"}
    for k, v in body.items():
        if k in allowed:
            setattr(sched, k, v)
    await db.flush()
    return ScheduleResponse.from_orm(sched)


@schedules_router.delete("/{schedule_id}", status_code=204,
                         dependencies=[Depends(require_permission(Permission.MANAGE_SCHEDULES))])
async def delete_schedule(schedule_id: uuid.UUID, db: AsyncSession = Depends(get_db)):
    sched = await db.get(AgentSchedule, schedule_id)
    if not sched:
        raise HTTPException(status_code=404, detail="Schedule not found")
    await db.delete(sched)


# ── Nodes router ──────────────────────────────────────────────────────────────

class NodeResponse(BaseModel):
    id: str
    node_id: str
    hostname: str
    address: str
    port: int
    status: str
    capacity: int
    current_load: int
    last_heartbeat: datetime

    @classmethod
    def from_orm(cls, obj: ExecutorNode) -> "NodeResponse":
        return cls(
            id=str(obj.id),
            node_id=obj.node_id,
            hostname=obj.hostname,
            address=obj.address,
            port=obj.port,
            status=obj.status,
            capacity=obj.capacity,
            current_load=obj.current_load,
            last_heartbeat=obj.last_heartbeat,
        )


class NodeHeartbeat(BaseModel):
    node_id: str
    hostname: str
    address: str
    port: int = 8081
    capacity: int = 50
    current_load: int = 0


@nodes_router.get("", response_model=list[NodeResponse])
async def list_nodes(db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        select(ExecutorNode).order_by(desc(ExecutorNode.last_heartbeat))
    )
    return [NodeResponse.from_orm(n) for n in result.scalars().all()]


@nodes_router.post("/heartbeat", status_code=200)
async def node_heartbeat(body: NodeHeartbeat, db: AsyncSession = Depends(get_db)):
    """Executor nodes call this every 10s to register their liveness."""
    result = await db.execute(
        select(ExecutorNode).where(ExecutorNode.node_id == body.node_id)
    )
    node = result.scalar_one_or_none()

    if node:
        node.hostname = body.hostname
        node.address = body.address
        node.port = body.port
        node.capacity = body.capacity
        node.current_load = body.current_load
        node.last_heartbeat = datetime.now(timezone.utc)
        node.status = "healthy"
    else:
        node = ExecutorNode(
            node_id=body.node_id,
            hostname=body.hostname,
            address=body.address,
            port=body.port,
            capacity=body.capacity,
            current_load=body.current_load,
            last_heartbeat=datetime.now(timezone.utc),
        )
        db.add(node)

    await db.flush()
    return {"status": "ok", "node_id": body.node_id}


@nodes_router.get("/{node_id}", response_model=NodeResponse)
async def get_node(node_id: str, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        select(ExecutorNode).where(ExecutorNode.node_id == node_id)
    )
    node = result.scalar_one_or_none()
    if not node:
        raise HTTPException(status_code=404, detail="Node not found")
    return NodeResponse.from_orm(node)