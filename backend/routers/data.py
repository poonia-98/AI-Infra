import uuid
from datetime import datetime, timezone, timedelta
from typing import Optional
from fastapi import APIRouter, Depends, Query, HTTPException
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import select, desc, func, and_
from database import get_db
from models import Log, Event, Metric, Execution, Alert, AlertRule, Container
from schemas import (
    LogCreate, LogResponse, EventCreate, EventResponse,
    MetricCreate, MetricResponse, ExecutionResponse,
    AlertResponse, AlertRuleCreate, AlertRuleResponse, ContainerResponse
)
from services.nats_service import nats_service
from services.redis_service import redis_service
from services.alert_service import alert_service
from database import AsyncSessionLocal

logs_router = APIRouter(prefix="/logs", tags=["logs"])
events_router = APIRouter(prefix="/events", tags=["events"])
metrics_router = APIRouter(prefix="/metrics", tags=["metrics"])
executions_router = APIRouter(prefix="/executions", tags=["executions"])
alerts_router = APIRouter(prefix="/alerts", tags=["alerts"])
containers_router = APIRouter(prefix="/containers", tags=["containers"])


def _parse_range(time_range: str | None) -> datetime | None:
    """Convert e.g. '5m', '1h', '7d' into a UTC datetime cutoff."""
    if not time_range:
        return None
    unit = time_range[-1]
    try:
        n = int(time_range[:-1])
    except Exception:
        return None
    delta_map = {"m": timedelta(minutes=n), "h": timedelta(hours=n), "d": timedelta(days=n)}
    delta = delta_map.get(unit)
    if not delta:
        return None
    return datetime.now(timezone.utc) - delta


# ─── Logs ─────────────────────────────────────────────────────────────────────
@logs_router.post("", response_model=LogResponse, status_code=201)
async def create_log(data: LogCreate, db: AsyncSession = Depends(get_db)):
    d = data.model_dump()
    d.pop("metadata", None)  # ORM attr is 'meta', not 'metadata'
    log = Log(**d)
    db.add(log)
    await db.flush()
    await db.refresh(log)
    log_dict = {
        "id": str(log.id), "agent_id": str(log.agent_id),
        "level": log.level, "message": log.message,
        "source": log.source, "timestamp": log.timestamp.isoformat(),
    }
    await redis_service.lpush_log(str(log.agent_id), log_dict)
    await nats_service.publish("logs.stream", log_dict)
    return log


@logs_router.get("", response_model=list[LogResponse])
async def list_logs(
    limit: int = Query(200, ge=1, le=2000),
    level: Optional[str] = None,
    time_range: Optional[str] = Query(None, description="e.g. 5m, 1h, 6h, 24h, 7d, 30d"),
    db: AsyncSession = Depends(get_db)
):
    cutoff = _parse_range(time_range)
    q = select(Log).order_by(desc(Log.timestamp)).limit(limit)
    if level:
        q = q.where(Log.level == level)
    if cutoff:
        q = q.where(Log.timestamp >= cutoff)
    result = await db.execute(q)
    return list(result.scalars().all())


# ─── Events ───────────────────────────────────────────────────────────────────
@events_router.post("", response_model=EventResponse, status_code=201)
async def create_event(data: EventCreate, db: AsyncSession = Depends(get_db)):
    event = Event(**data.model_dump())
    db.add(event)
    await db.flush()
    await db.refresh(event)
    event_dict = {
        "id": str(event.id), "agent_id": str(event.agent_id),
        "event_type": event.event_type, "payload": event.payload,
        "source": event.source, "level": event.level,
        "created_at": event.created_at.isoformat(),
    }
    await nats_service.publish("events.stream", event_dict)
    return event


@events_router.get("", response_model=list[EventResponse])
async def list_events(
    limit: int = Query(200, ge=1, le=2000),
    event_type: Optional[str] = None,
    time_range: Optional[str] = Query(None),
    db: AsyncSession = Depends(get_db)
):
    cutoff = _parse_range(time_range)
    q = select(Event).order_by(desc(Event.created_at)).limit(limit)
    if event_type:
        q = q.where(Event.event_type == event_type)
    if cutoff:
        q = q.where(Event.created_at >= cutoff)
    result = await db.execute(q)
    return list(result.scalars().all())


# ─── Metrics ──────────────────────────────────────────────────────────────────
@metrics_router.post("", response_model=MetricResponse, status_code=201)
async def create_metric(data: MetricCreate, db: AsyncSession = Depends(get_db)):
    metric = Metric(**data.model_dump())
    db.add(metric)
    await db.flush()
    await db.refresh(metric)
    payload = {
        "agent_id": str(metric.agent_id), "metric_name": metric.metric_name,
        "metric_value": metric.metric_value, "labels": metric.labels,
        "timestamp": metric.timestamp.isoformat(),
    }
    await nats_service.publish("metrics.stream", payload)
    # Evaluate alerts in background
    async with AsyncSessionLocal() as alert_db:
        try:
            await alert_service.evaluate_metric(alert_db, str(metric.agent_id), metric.metric_name, metric.metric_value)
            await alert_db.commit()
        except Exception:
            await alert_db.rollback()
    return metric


@metrics_router.get("", response_model=list[MetricResponse])
async def list_metrics(
    limit: int = Query(200, ge=1, le=2000),
    metric_name: Optional[str] = None,
    time_range: Optional[str] = Query(None),
    db: AsyncSession = Depends(get_db)
):
    cutoff = _parse_range(time_range)
    q = select(Metric).order_by(desc(Metric.timestamp)).limit(limit)
    if metric_name:
        q = q.where(Metric.metric_name == metric_name)
    if cutoff:
        q = q.where(Metric.timestamp >= cutoff)
    result = await db.execute(q)
    return list(result.scalars().all())


@metrics_router.get("/timeseries")
async def metrics_timeseries(
    metric_name: str = Query(...),
    agent_id: Optional[str] = None,
    time_range: str = Query("1h"),
    bucket_seconds: int = Query(60, ge=5, le=3600),
    db: AsyncSession = Depends(get_db)
):
    """Return time-bucketed metric data for charting."""
    cutoff = _parse_range(time_range) or datetime.now(timezone.utc) - timedelta(hours=1)
    q = (
        select(Metric)
        .where(Metric.metric_name == metric_name, Metric.timestamp >= cutoff)
        .order_by(Metric.timestamp.asc())
        .limit(5000)
    )
    if agent_id:
        q = q.where(Metric.agent_id == uuid.UUID(agent_id))
    result = await db.execute(q)
    rows = result.scalars().all()
    # Bucket by time
    buckets: dict[str, list[float]] = {}
    for row in rows:
        bucket_ts = row.timestamp.replace(
            second=(row.timestamp.second // bucket_seconds) * bucket_seconds,
            microsecond=0
        )
        key = bucket_ts.isoformat()
        buckets.setdefault(key, []).append(row.metric_value)
    points = [
        {"timestamp": k, "value": sum(v) / len(v)}
        for k, v in sorted(buckets.items())
    ]
    return {"metric_name": metric_name, "time_range": time_range, "points": points}


# ─── Executions ───────────────────────────────────────────────────────────────
@executions_router.post("", response_model=ExecutionResponse, status_code=201)
async def create_execution(data: dict, db: AsyncSession = Depends(get_db)):
    from models import Execution as Exec
    exec_obj = Exec(
        agent_id=uuid.UUID(data["agent_id"]),
        status=data.get("status", "pending"),
        input=data.get("input", {}),
        started_at=datetime.now(timezone.utc) if data.get("status") == "running" else None,
    )
    db.add(exec_obj)
    await db.flush()
    await db.refresh(exec_obj)
    return exec_obj


@executions_router.get("", response_model=list[ExecutionResponse])
async def list_executions(
    limit: int = Query(100, ge=1, le=1000),
    status: Optional[str] = None,
    time_range: Optional[str] = Query(None),
    db: AsyncSession = Depends(get_db)
):
    cutoff = _parse_range(time_range)
    q = select(Execution).order_by(desc(Execution.created_at)).limit(limit)
    if status:
        q = q.where(Execution.status == status)
    if cutoff:
        q = q.where(Execution.created_at >= cutoff)
    result = await db.execute(q)
    return list(result.scalars().all())


@executions_router.get("/{execution_id}", response_model=ExecutionResponse)
async def get_execution(execution_id: uuid.UUID, db: AsyncSession = Depends(get_db)):
    result = await db.execute(select(Execution).where(Execution.id == execution_id))
    execution = result.scalar_one_or_none()
    if not execution:
        raise HTTPException(status_code=404, detail="Execution not found")
    return execution


# ─── Alerts ───────────────────────────────────────────────────────────────────
@alerts_router.get("", response_model=list[AlertResponse])
async def list_alerts(
    resolved: Optional[bool] = None,
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db)
):
    return await alert_service.list_alerts(db, resolved=resolved, limit=limit)


@alerts_router.post("/{alert_id}/resolve", response_model=AlertResponse)
async def resolve_alert(alert_id: uuid.UUID, db: AsyncSession = Depends(get_db)):
    alert = await alert_service.resolve_alert(db, alert_id)
    if not alert:
        raise HTTPException(status_code=404, detail="Alert not found")
    return alert


@alerts_router.get("/rules", response_model=list[AlertRuleResponse])
async def list_alert_rules(db: AsyncSession = Depends(get_db)):
    rules = await alert_service.list_rules(db)
    return [AlertRuleResponse.from_orm(r) for r in rules]


@alerts_router.post("/rules", response_model=AlertRuleResponse, status_code=201)
async def create_alert_rule(data: AlertRuleCreate, db: AsyncSession = Depends(get_db)):
    return await alert_service.create_rule(db, data)


# ─── Containers ───────────────────────────────────────────────────────────────
@containers_router.get("", response_model=list[ContainerResponse])
async def list_containers(
    agent_id: Optional[str] = None,
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db)
):
    q = select(Container).order_by(desc(Container.created_at)).limit(limit)
    if agent_id and agent_id.strip():
        try:
            q = q.where(Container.agent_id == uuid.UUID(agent_id))
        except ValueError:
            pass
    result = await db.execute(q)
    return [ContainerResponse.from_orm(c) for c in result.scalars().all()]