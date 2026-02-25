import uuid
import json
import httpx
from datetime import datetime, timezone
from typing import Any, Optional
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import select, func, update
from models import Agent, Execution, Log, Event, Metric, Container
from schemas import AgentCreate, AgentUpdate
from services.redis_service import redis_service
from services.nats_service import nats_service
from config import settings
import structlog

logger = structlog.get_logger()


class AgentService:
    async def create_agent(self, db: AsyncSession, data: AgentCreate) -> Agent:
        agent = Agent(
            id=uuid.uuid4(),
            name=data.name,
            description=data.description,
            agent_type=data.agent_type,
            config=data.config,
            image=data.image,
            status="created",
            cpu_limit=data.cpu_limit,
            memory_limit=data.memory_limit,
            env_vars=data.env_vars,
            labels=data.labels,
            auto_restart=data.auto_restart,
        )
        db.add(agent)
        await db.flush()
        await db.refresh(agent)
        if redis_service.client is None:
            await redis_service.connect()
        await redis_service.set_agent_state(str(agent.id), {
            "status": "created", "created_at": agent.created_at.isoformat(),
        })
        await nats_service.publish("agents.events", {
            "event_type": "agent.created", "agent_id": str(agent.id),
            "name": agent.name, "source": "control-plane",
            "timestamp": datetime.now(timezone.utc).isoformat(), "level": "info",
        })
        return agent

    async def get_agent(self, db: AsyncSession, agent_id: uuid.UUID) -> Optional[Agent]:
        result = await db.execute(select(Agent).where(Agent.id == agent_id))
        return result.scalar_one_or_none()

    async def list_agents(self, db: AsyncSession, skip: int = 0, limit: int = 100) -> list[Agent]:
        result = await db.execute(
            select(Agent).offset(skip).limit(limit).order_by(Agent.created_at.desc())
        )
        return list(result.scalars().all())

    async def update_agent(self, db: AsyncSession, agent_id: uuid.UUID, data: AgentUpdate) -> Optional[Agent]:
        agent = await self.get_agent(db, agent_id)
        if not agent:
            return None
        update_data = data.model_dump(exclude_unset=True)
        for key, value in update_data.items():
            setattr(agent, key, value)
        agent.updated_at = datetime.now(timezone.utc)
        await db.flush()
        await db.refresh(agent)
        return agent

    async def delete_agent(self, db: AsyncSession, agent_id: uuid.UUID) -> bool:
        agent = await self.get_agent(db, agent_id)
        if not agent:
            return False
        if agent.status == "running":
            await self.stop_agent(db, agent_id)
        await db.delete(agent)
        await redis_service.delete_agent_state(str(agent_id))
        await nats_service.publish("agents.events", {
            "event_type": "agent.deleted", "agent_id": str(agent_id),
            "source": "control-plane", "level": "info",
            "timestamp": datetime.now(timezone.utc).isoformat(),
        })
        return True

    async def start_agent(self, db: AsyncSession, agent_id: uuid.UUID) -> dict[str, Any]:
        agent = await self.get_agent(db, agent_id)
        if not agent:
            return {"success": False, "error": "Agent not found"}

        try:
            async with httpx.AsyncClient(timeout=30.0) as client:
                response = await client.post(
                    f"{settings.EXECUTOR_URL}/containers/start",
                    json={
                        "agent_id": str(agent_id),
                        "image": agent.image,
                        "name": f"agent-{agent.name.lower().replace(' ', '-')}-{str(agent_id)[:8]}",
                        "cpu_limit": agent.cpu_limit,
                        "memory_limit": agent.memory_limit,
                        "env_vars": {**agent.env_vars, "AGENT_ID": str(agent_id), "AGENT_NAME": agent.name},
                        "labels": {**agent.labels, "platform.agent_id": str(agent_id)},
                    }
                )
                result = response.json()

            if result.get("success"):
                # Ensure Redis is connected before using it
                if redis_service.client is None:
                    await redis_service.connect()

                stmt = update(Agent).where(Agent.id == agent_id).values(
                    status="running",
                    container_id=result.get("container_id"),
                    container_name=result.get("container_name"),
                    updated_at=datetime.now(timezone.utc),
                )
                await db.execute(stmt)

                # Track container record
                container = Container(
                    agent_id=agent_id,
                    container_id=result.get("container_id", ""),
                    container_name=result.get("container_name"),
                    image=agent.image,
                    status="running",
                    started_at=datetime.now(timezone.utc),
                )
                db.add(container)

                await redis_service.set_agent_state(str(agent_id), {
                    "status": "running",
                    "container_id": result.get("container_id"),
                    "started_at": datetime.now(timezone.utc).isoformat(),
                })

                event = Event(
                    agent_id=agent_id, event_type="agent.started",
                    payload={"container_id": result.get("container_id")},
                    source="control-plane", level="info",
                )
                db.add(event)

                await nats_service.publish("agents.events", {
                    "event_type": "agent.started", "agent_id": str(agent_id),
                    "container_id": result.get("container_id"),
                    "source": "control-plane", "level": "info",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                })

            return result
        except httpx.ConnectError:
            await self._update_agent_status(db, agent_id, "error")
            return {"success": False, "error": "Executor service unavailable"}
        except Exception as e:
            logger.error("Failed to start agent", agent_id=str(agent_id), error=str(e))
            return {"success": False, "error": str(e)}

    async def stop_agent(self, db: AsyncSession, agent_id: uuid.UUID) -> dict[str, Any]:
        agent = await self.get_agent(db, agent_id)
        if not agent:
            return {"success": False, "error": "Agent not found"}

        try:
            async with httpx.AsyncClient(timeout=30.0) as client:
                response = await client.post(
                    f"{settings.EXECUTOR_URL}/containers/stop",
                    json={"agent_id": str(agent_id), "container_id": agent.container_id}
                )
                result = response.json()

            if result.get("success"):
                # Ensure Redis is connected before using it
                if redis_service.client is None:
                    await redis_service.connect()

                await self._update_agent_status(db, agent_id, "stopped")
                # Update container record
                from sqlalchemy import update as sa_update
                stmt = sa_update(Container).where(
                    Container.agent_id == agent_id,
                    Container.status == "running"
                ).values(status="stopped", stopped_at=datetime.now(timezone.utc))
                await db.execute(stmt)

                await redis_service.set_agent_state(str(agent_id), {
                    "status": "stopped", "stopped_at": datetime.now(timezone.utc).isoformat(),
                })
                event = Event(
                    agent_id=agent_id, event_type="agent.stopped",
                    payload={}, source="control-plane", level="info",
                )
                db.add(event)
                await nats_service.publish("agents.events", {
                    "event_type": "agent.stopped", "agent_id": str(agent_id),
                    "source": "control-plane", "level": "info",
                    "timestamp": datetime.now(timezone.utc).isoformat(),
                })

            return result
        except Exception as e:
            return {"success": False, "error": str(e)}

    async def restart_agent(self, db: AsyncSession, agent_id: uuid.UUID) -> dict[str, Any]:
        stop_result = await self.stop_agent(db, agent_id)
        if not stop_result.get("success") and "not found" not in stop_result.get("error", "").lower():
            return stop_result
        return await self.start_agent(db, agent_id)

    async def handle_agent_failure(self, db: AsyncSession, agent_id_str: str):
        """Called when agent.failed event arrives; auto-restart if configured."""
        try:
            agent_id = uuid.UUID(agent_id_str)
            agent = await self.get_agent(db, agent_id)
            if not agent:
                return
            stmt = update(Agent).where(Agent.id == agent_id).values(
                status="error",
                last_error=f"Agent failed at {datetime.now(timezone.utc).isoformat()}",
                updated_at=datetime.now(timezone.utc),
            )
            await db.execute(stmt)
            await db.commit()

            if agent.auto_restart and agent.restart_count < 5:
                import asyncio
                await asyncio.sleep(5)
                from database import AsyncSessionLocal
                async with AsyncSessionLocal() as restart_db:
                    stmt2 = update(Agent).where(Agent.id == agent_id).values(
                        restart_count=Agent.restart_count + 1
                    )
                    await restart_db.execute(stmt2)
                    await self.start_agent(restart_db, agent_id)
                    await restart_db.commit()
                    logger.info("Auto-restarted agent", agent_id=agent_id_str)
        except Exception as e:
            logger.error("Auto-restart failed", agent_id=agent_id_str, error=str(e))

    async def _update_agent_status(self, db: AsyncSession, agent_id: uuid.UUID, status: str):
        from sqlalchemy import update as sa_update
        stmt = sa_update(Agent).where(Agent.id == agent_id).values(
            status=status, updated_at=datetime.now(timezone.utc)
        )
        await db.execute(stmt)

    async def get_system_health(self, db: AsyncSession) -> dict[str, Any]:
        total_agents = await db.scalar(select(func.count(Agent.id)))
        running_agents = await db.scalar(select(func.count(Agent.id)).where(Agent.status == "running"))
        failed_agents = await db.scalar(select(func.count(Agent.id)).where(Agent.status == "error"))
        total_executions = await db.scalar(select(func.count(Execution.id)))
        active_executions = await db.scalar(
            select(func.count(Execution.id)).where(Execution.status.in_(["pending", "running"]))
        )
        total_alerts = await db.scalar(select(func.count()).select_from(__import__('models').Alert).where(__import__('models').Alert.resolved == False))

        redis_ok = await redis_service.ping()
        nats_ok = nats_service.is_connected()

        return {
            "status": "healthy",
            "total_agents": total_agents or 0,
            "running_agents": running_agents or 0,
            "failed_agents": failed_agents or 0,
            "total_executions": total_executions or 0,
            "active_executions": active_executions or 0,
            "active_alerts": total_alerts or 0,
            "services": {
                "database": "healthy",
                "redis": "healthy" if redis_ok else "degraded",
                "nats": "healthy" if nats_ok else "degraded",
            },
        }


agent_service = AgentService()