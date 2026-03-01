"""
Control Brain Client Service
Proxies backend calls to the Control Brain for state, service registry,
placement decisions, and orchestration commands.
"""
from __future__ import annotations
import json
from typing import Any, Optional
import httpx
import structlog
from config import settings

logger = structlog.get_logger()

BRAIN_URL = getattr(settings, "CONTROL_BRAIN_URL", "http://control-brain:8200")
TIMEOUT = 5.0


class BrainService:
    def __init__(self):
        self._client: Optional[httpx.AsyncClient] = None

    async def client(self) -> httpx.AsyncClient:
        if self._client is None or self._client.is_closed:
            self._client = httpx.AsyncClient(base_url=BRAIN_URL, timeout=TIMEOUT)
        return self._client

    async def close(self):
        if self._client and not self._client.is_closed:
            await self._client.aclose()

    # ── State ──────────────────────────────────────────────────────────────────

    async def get_state(self) -> dict:
        try:
            c = await self.client()
            resp = await c.get("/api/v1/brain/state")
            resp.raise_for_status()
            return resp.json()
        except Exception as e:
            logger.warning("brain.get_state failed", error=str(e))
            return {"error": str(e), "available": False}

    async def get_summary(self) -> dict:
        try:
            c = await self.client()
            resp = await c.get("/api/v1/brain/state/summary")
            resp.raise_for_status()
            return resp.json()
        except Exception as e:
            return {"error": str(e)}

    async def get_agents(self) -> list:
        try:
            c = await self.client()
            resp = await c.get("/api/v1/brain/state/agents")
            resp.raise_for_status()
            return resp.json().get("agents", [])
        except Exception:
            return []

    # ── Service registry ───────────────────────────────────────────────────────

    async def get_services(self) -> list:
        try:
            c = await self.client()
            resp = await c.get("/api/v1/brain/services")
            resp.raise_for_status()
            return resp.json()
        except Exception as e:
            logger.warning("brain.get_services failed", error=str(e))
            return []

    async def register_service(
        self,
        service_name: str,
        service_url: str,
        health_endpoint: str = "",
        capabilities: list[str] | None = None,
        node: str = "",
        version: str = "1.0.0",
    ) -> bool:
        try:
            c = await self.client()
            resp = await c.post("/api/v1/brain/services/register", json={
                "service_name": service_name,
                "service_url": service_url,
                "health_endpoint": health_endpoint or f"{service_url}/health",
                "capabilities": capabilities or [],
                "node": node,
                "version": version,
            })
            return resp.status_code in (200, 201)
        except Exception as e:
            logger.warning("brain.register_service failed", error=str(e))
            return False

    # ── Nodes ──────────────────────────────────────────────────────────────────

    async def get_nodes(self) -> list:
        try:
            c = await self.client()
            resp = await c.get("/api/v1/brain/nodes")
            resp.raise_for_status()
            return resp.json()
        except Exception:
            return []

    # ── Decisions ──────────────────────────────────────────────────────────────

    async def get_decisions(self) -> list:
        try:
            c = await self.client()
            resp = await c.get("/api/v1/brain/decisions")
            resp.raise_for_status()
            return resp.json().get("decisions", [])
        except Exception:
            return []

    async def issue_decision(
        self,
        decision_type: str,
        agent_id: str = "",
        service_name: str = "",
        reason: str = "",
        action: dict | None = None,
    ) -> bool:
        try:
            c = await self.client()
            resp = await c.post("/api/v1/brain/decisions", json={
                "decision_type": decision_type,
                "agent_id": agent_id,
                "service_name": service_name,
                "reason": reason,
                "action": action or {},
            })
            return resp.status_code == 202
        except Exception as e:
            logger.warning("brain.issue_decision failed", error=str(e))
            return False

    # ── Placement ──────────────────────────────────────────────────────────────

    async def get_placement(
        self,
        cpu_request: float = 0.5,
        memory_request_mb: float = 256,
        region: str = "",
    ) -> dict:
        try:
            c = await self.client()
            resp = await c.post("/api/v1/brain/placement", json={
                "cpu_request": cpu_request,
                "memory_request_mb": memory_request_mb,
                "region": region,
            })
            resp.raise_for_status()
            return resp.json()
        except Exception:
            return {"node_id": "default", "region": region or "local"}

    # ── Scaling ────────────────────────────────────────────────────────────────

    async def scale_agent(self, agent_id: str, direction: str, replicas: int = 1, reason: str = "") -> bool:
        try:
            c = await self.client()
            resp = await c.post("/api/v1/brain/scale", json={
                "agent_id": agent_id,
                "direction": direction,
                "replicas": replicas,
                "reason": reason,
            })
            return resp.status_code == 202
        except Exception:
            return False

    # ── Reconcile trigger ──────────────────────────────────────────────────────

    async def trigger_reconcile(self) -> bool:
        try:
            c = await self.client()
            resp = await c.post("/api/v1/brain/reconcile")
            return resp.status_code == 202
        except Exception:
            return False

    async def health(self) -> bool:
        try:
            c = await self.client()
            resp = await c.get("/health", timeout=2.0)
            return resp.status_code == 200
        except Exception:
            return False


brain_service = BrainService()