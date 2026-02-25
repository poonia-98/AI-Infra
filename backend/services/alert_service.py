"""Alert engine: evaluates metrics against rules and fires alerts."""
import uuid
from datetime import datetime, timezone
from typing import Any
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import select
from models import AlertRule, Alert, Agent
from services.nats_service import nats_service
import structlog

logger = structlog.get_logger()

CONDITION_FNS = {
    "gt":  lambda v, t: v > t,
    "lt":  lambda v, t: v < t,
    "gte": lambda v, t: v >= t,
    "lte": lambda v, t: v <= t,
    "eq":  lambda v, t: v == t,
}

# In-memory cooldown: rule_id+agent_id → last_fired_ts
_cooldown: dict[str, float] = {}
COOLDOWN_SECONDS = 300  # 5 min per rule per agent


class AlertService:
    async def evaluate_metric(self, db: AsyncSession, agent_id: str, metric_name: str, metric_value: float):
        """Called when a metric arrives; fires alerts if rules match."""
        try:
            result = await db.execute(
                select(AlertRule).where(
                    AlertRule.enabled == True,
                    AlertRule.metric_name == metric_name,
                )
            )
            rules = result.scalars().all()

            for rule in rules:
                # Rule is global or targets this specific agent
                if rule.agent_id is not None and str(rule.agent_id) != agent_id:
                    continue

                check_fn = CONDITION_FNS.get(rule.condition)
                if not check_fn:
                    continue
                if not check_fn(metric_value, rule.threshold):
                    continue

                # Cooldown check
                key = f"{rule.id}:{agent_id}"
                import time
                now = time.time()
                if key in _cooldown and (now - _cooldown[key]) < COOLDOWN_SECONDS:
                    continue
                _cooldown[key] = now

                # Create alert
                msg = (
                    f"[{rule.severity.upper()}] {rule.name}: "
                    f"{metric_name}={metric_value:.2f} {rule.condition} {rule.threshold:.2f} "
                    f"for agent {agent_id[:8]}"
                )
                alert = Alert(
                    rule_id=rule.id,
                    agent_id=uuid.UUID(agent_id) if agent_id else None,
                    metric_name=metric_name,
                    metric_value=metric_value,
                    threshold=rule.threshold,
                    severity=rule.severity,
                    message=msg,
                )
                db.add(alert)
                await db.flush()

                await nats_service.publish("agents.events", {
                    "event_type": "alert.fired",
                    "agent_id": agent_id,
                    "alert_id": str(alert.id),
                    "severity": rule.severity,
                    "message": msg,
                    "metric_name": metric_name,
                    "metric_value": metric_value,
                    "threshold": rule.threshold,
                    "source": "alert-engine",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                    "level": "warn",
                })
                logger.warning("Alert fired", rule=rule.name, agent=agent_id, value=metric_value)
        except Exception as e:
            logger.error("Alert evaluation error", error=str(e))

    async def list_alerts(self, db: AsyncSession, resolved: bool | None = None, limit: int = 100) -> list[Alert]:
        q = select(Alert).order_by(Alert.created_at.desc()).limit(limit)
        if resolved is not None:
            q = q.where(Alert.resolved == resolved)
        result = await db.execute(q)
        return list(result.scalars().all())

    async def resolve_alert(self, db: AsyncSession, alert_id: uuid.UUID) -> Alert | None:
        result = await db.execute(select(Alert).where(Alert.id == alert_id))
        alert = result.scalar_one_or_none()
        if alert:
            alert.resolved = True
            alert.resolved_at = datetime.now(timezone.utc)
            await db.flush()
        return alert

    async def list_rules(self, db: AsyncSession) -> list[AlertRule]:
        result = await db.execute(select(AlertRule).order_by(AlertRule.created_at.desc()))
        return list(result.scalars().all())

    async def create_rule(self, db: AsyncSession, data: Any) -> AlertRule:
        rule = AlertRule(**data.model_dump())
        db.add(rule)
        await db.flush()
        await db.refresh(rule)
        return rule


alert_service = AlertService()