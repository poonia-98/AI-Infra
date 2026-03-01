"""
Smart Redis Cache Service — replaces database polling with event-driven cache.

Cache topology:
  agent:state:{id}        → live agent status  (TTL 300s)
  agent:list:{org}        → paginated agent list (TTL 30s, busted on writes)
  agent:logs:{id}         → recent logs ring buffer (1000 entries, TTL 24h)
  agent:metrics:{id}      → metric snapshots ring buffer (TTL 24h)
  events:stream:{org}     → latest events (TTL 1h)
  brain:summary           → control-brain summary (TTL 10s)
  services:health         → service registry (TTL 15s)
  alerts:active:{org}     → active alerts (TTL 60s)
"""
from __future__ import annotations
import json
import time
from typing import Any, Optional, List
import structlog

logger = structlog.get_logger()

# TTLs (seconds)
TTL_AGENT_STATE   = 300
TTL_AGENT_LIST    = 30
TTL_LOGS          = 86400
TTL_METRICS       = 86400
TTL_EVENTS        = 3600
TTL_BRAIN_SUMMARY = 10
TTL_SERVICES      = 15
TTL_ALERTS        = 60
TTL_EXECUTION     = 3600
MAX_RING          = 1000   # max items in ring buffers


class CacheService:
    """Thin wrapper — requires redis_service to be connected."""

    def __init__(self, redis):
        self._r = redis

    # ── Agent state ────────────────────────────────────────────────────────────

    async def set_agent(self, agent_id: str, state: dict):
        await self._set(f"agent:state:{agent_id}", state, TTL_AGENT_STATE)

    async def get_agent(self, agent_id: str) -> Optional[dict]:
        return await self._get(f"agent:state:{agent_id}")

    async def del_agent(self, agent_id: str):
        await self._del(f"agent:state:{agent_id}")

    async def set_agent_status(self, agent_id: str, status: str):
        """Patch only the status field of a cached agent."""
        existing = await self.get_agent(agent_id)
        if existing:
            existing["status"] = status
            await self.set_agent(agent_id, existing)

    # ── Agent list cache ───────────────────────────────────────────────────────

    async def set_agent_list(self, org_id: str, agents: list, skip: int = 0, limit: int = 100):
        key = f"agent:list:{org_id}:{skip}:{limit}"
        await self._set(key, agents, TTL_AGENT_LIST)

    async def get_agent_list(self, org_id: str, skip: int = 0, limit: int = 100) -> Optional[list]:
        return await self._get(f"agent:list:{org_id}:{skip}:{limit}")

    async def bust_agent_list(self, org_id: str):
        """Invalidate all agent list pages for an org."""
        await self._del_pattern(f"agent:list:{org_id}:*")

    # ── Logs ring buffer ───────────────────────────────────────────────────────

    async def push_log(self, agent_id: str, log: dict):
        key = f"agent:logs:{agent_id}"
        c = self._r._client
        if not c: return
        try:
            await c.lpush(key, json.dumps(log, default=str))
            await c.ltrim(key, 0, MAX_RING - 1)
            await c.expire(key, TTL_LOGS)
        except Exception as e:
            logger.debug("cache.push_log failed", error=str(e))

    async def get_logs(self, agent_id: str, limit: int = 200) -> list:
        c = self._r._client
        if not c: return []
        try:
            items = await c.lrange(f"agent:logs:{agent_id}", 0, min(limit, MAX_RING) - 1)
            return [json.loads(i) for i in items]
        except Exception:
            return []

    # ── Metrics ring buffer ────────────────────────────────────────────────────

    async def push_metric(self, agent_id: str, metric: dict):
        key = f"agent:metrics:{agent_id}"
        c = self._r._client
        if not c: return
        try:
            await c.lpush(key, json.dumps(metric, default=str))
            await c.ltrim(key, 0, MAX_RING - 1)
            await c.expire(key, TTL_METRICS)
        except Exception as e:
            logger.debug("cache.push_metric failed", error=str(e))

    async def get_metrics(self, agent_id: str, limit: int = 200) -> list:
        c = self._r._client
        if not c: return []
        try:
            items = await c.lrange(f"agent:metrics:{agent_id}", 0, min(limit, MAX_RING) - 1)
            return [json.loads(i) for i in items]
        except Exception:
            return []

    # ── Events ────────────────────────────────────────────────────────────────

    async def push_event(self, org_id: str, event: dict):
        key = f"events:stream:{org_id}"
        c = self._r._client
        if not c: return
        try:
            await c.lpush(key, json.dumps(event, default=str))
            await c.ltrim(key, 0, MAX_RING - 1)
            await c.expire(key, TTL_EVENTS)
        except Exception:
            pass

    async def get_events(self, org_id: str, limit: int = 100) -> list:
        c = self._r._client
        if not c: return []
        try:
            items = await c.lrange(f"events:stream:{org_id}", 0, min(limit, MAX_RING) - 1)
            return [json.loads(i) for i in items]
        except Exception:
            return []

    # ── Brain summary ──────────────────────────────────────────────────────────

    async def set_brain_summary(self, summary: dict):
        await self._set("brain:summary", summary, TTL_BRAIN_SUMMARY)

    async def get_brain_summary(self) -> Optional[dict]:
        return await self._get("brain:summary")

    # ── Services health ────────────────────────────────────────────────────────

    async def set_services(self, services: list):
        await self._set("services:health", services, TTL_SERVICES)

    async def get_services(self) -> Optional[list]:
        return await self._get("services:health")

    # ── Active alerts ──────────────────────────────────────────────────────────

    async def set_alerts(self, org_id: str, alerts: list):
        await self._set(f"alerts:active:{org_id}", alerts, TTL_ALERTS)

    async def get_alerts(self, org_id: str) -> Optional[list]:
        return await self._get(f"alerts:active:{org_id}")

    async def bust_alerts(self, org_id: str):
        await self._del(f"alerts:active:{org_id}")

    # ── Generic operations ─────────────────────────────────────────────────────

    async def _set(self, key: str, value: Any, ttl: int):
        c = self._r._client
        if not c: return
        try:
            await c.setex(key, ttl, json.dumps(value, default=str))
        except Exception as e:
            logger.debug("cache._set failed", key=key, error=str(e))

    async def _get(self, key: str) -> Optional[Any]:
        c = self._r._client
        if not c: return None
        try:
            data = await c.get(key)
            return json.loads(data) if data else None
        except Exception:
            return None

    async def _del(self, key: str):
        c = self._r._client
        if not c: return
        try:
            await c.delete(key)
        except Exception:
            pass

    async def _del_pattern(self, pattern: str):
        c = self._r._client
        if not c: return
        try:
            keys = await c.keys(pattern)
            if keys:
                await c.delete(*keys)
        except Exception:
            pass


# Initialized in main.py after redis_service connects
cache: Optional[CacheService] = None


def init_cache(redis_service):
    global cache
    cache = CacheService(redis_service)
    logger.info("cache service initialized")
    return cache