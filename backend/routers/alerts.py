import uuid
from datetime import datetime, timezone, timedelta
from fastapi import APIRouter, Depends, Query, HTTPException
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import select, desc, and_, func
from database import get_db
from models import Alert, Metric, Agent
from schemas import AlertCreate, AlertResponse
from services.alert_service import alert_service

alerts_router = APIRouter(prefix="/alerts", tags=["alerts"])


@alerts_router.get("", response_model=list[AlertResponse])
async def list_alerts(
    status: str | None = None,
    limit: int = Query(100, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
):
    return await alert_service.list_alerts(db, status=status, limit=limit)


@alerts_router.get("/counts")
async def alert_counts(db: AsyncSession = Depends(get_db)):
    return await alert_service.get_alert_counts(db)


@alerts_router.post("/{alert_id}/resolve", response_model=AlertResponse)
async def resolve_alert(alert_id: uuid.UUID, db: AsyncSession = Depends(get_db)):
    ok = await alert_service.resolve_alert(db, alert_id)
    if not ok:
        raise HTTPException(404, "Alert not found or already resolved")
    result = await db.execute(select(Alert).where(Alert.id == alert_id))
    return result.scalar_one()


# ─── Historical metrics with time range support ───────────────────────────────
analytics_router = APIRouter(prefix="/analytics", tags=["analytics"])


@analytics_router.get("/metrics/history")
async def metrics_history(
    agent_id:    str | None = None,
    metric_name: str | None = None,
    range:       str = Query("1h", regex="^(5m|15m|1h|6h|24h|7d|30d)$"),
    limit:       int = Query(500, ge=1, le=5000),
    db:          AsyncSession = Depends(get_db),
):
    """Return historical metric time-series for charting."""
    delta = {
        "5m":  timedelta(minutes=5),
        "15m": timedelta(minutes=15),
        "1h":  timedelta(hours=1),
        "6h":  timedelta(hours=6),
        "24h": timedelta(hours=24),
        "7d":  timedelta(days=7),
        "30d": timedelta(days=30),
    }[range]
    since = datetime.now(timezone.utc) - delta

    q = select(Metric).where(Metric.timestamp >= since)
    if agent_id:
        q = q.where(Metric.agent_id == uuid.UUID(agent_id))
    if metric_name:
        q = q.where(Metric.metric_name == metric_name)
    q = q.order_by(desc(Metric.timestamp)).limit(limit)

    result = await db.execute(q)
    rows = list(result.scalars().all())

    return [
        {
            "agent_id":     str(m.agent_id),
            "metric_name":  m.metric_name,
            "metric_value": m.metric_value,
            "timestamp":    m.timestamp.isoformat(),
        }
        for m in rows
    ]


@analytics_router.get("/events/history")
async def events_history(
    range: str = Query("1h", regex="^(5m|15m|1h|6h|24h|7d|30d)$"),
    limit: int = Query(500, ge=1, le=5000),
    db:    AsyncSession = Depends(get_db),
):
    from models import Event
    delta = {
        "5m": timedelta(minutes=5), "15m": timedelta(minutes=15),
        "1h": timedelta(hours=1),   "6h":  timedelta(hours=6),
        "24h": timedelta(hours=24), "7d":  timedelta(days=7),
        "30d": timedelta(days=30),
    }[range]
    since = datetime.now(timezone.utc) - delta
    result = await db.execute(
        select(Event).where(Event.created_at >= since).order_by(desc(Event.created_at)).limit(limit)
    )
    rows = list(result.scalars().all())
    return [
        {"id": str(e.id), "agent_id": str(e.agent_id), "event_type": e.event_type,
         "payload": e.payload, "source": e.source, "created_at": e.created_at.isoformat()}
        for e in rows
    ]


@analytics_router.get("/summary")
async def analytics_summary(db: AsyncSession = Depends(get_db)):
    """Aggregate stats for overview cards."""
    from models import Execution, Log
    total_agents   = await db.scalar(select(func.count(Agent.id)))
    running        = await db.scalar(select(func.count(Agent.id)).where(Agent.status == "running"))
    failed         = await db.scalar(select(func.count(Agent.id)).where(Agent.status == "error"))
    total_execs    = await db.scalar(select(func.count(Execution.id)))
    active_execs   = await db.scalar(select(func.count(Execution.id)).where(Execution.status == "running"))
    total_logs     = await db.scalar(select(func.count(Log.id)))
    alert_counts   = await alert_service.get_alert_counts(db)
    return {
        "total_agents":     total_agents or 0,
        "running_agents":   running or 0,
        "failed_agents":    failed or 0,
        "total_executions": total_execs or 0,
        "active_executions": active_execs or 0,
        "total_logs":       total_logs or 0,
        "alerts":           alert_counts,
    }