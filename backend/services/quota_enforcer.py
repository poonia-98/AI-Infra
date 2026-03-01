import re
from typing import Any
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text


def parse_memory_to_mib(value: str) -> int:
    if not value:
        return 0
    v = value.strip().lower()
    match = re.match(r"^(\d+(?:\.\d+)?)([kmgt]?i?b?)?$", v)
    if not match:
        return 0
    number = float(match.group(1))
    unit = (match.group(2) or "m").replace("ib", "i")
    factors = {
        "k": 1 / 1024,
        "ki": 1 / 1024,
        "m": 1,
        "mi": 1,
        "g": 1024,
        "gi": 1024,
        "t": 1024 * 1024,
        "ti": 1024 * 1024,
        "b": 1 / (1024 * 1024),
    }
    return int(number * factors.get(unit, 1))


class QuotaEnforcer:
    async def ensure_rows(self, db: AsyncSession, organisation_id: str) -> None:
        await db.execute(
            text(
                """
                INSERT INTO organisation_quotas (organisation_id)
                VALUES (:organisation_id::uuid)
                ON CONFLICT (organisation_id) DO NOTHING
                """
            ),
            {"organisation_id": organisation_id},
        )
        await db.execute(
            text(
                """
                INSERT INTO organisation_usage (organisation_id)
                VALUES (:organisation_id::uuid)
                ON CONFLICT (organisation_id) DO NOTHING
                """
            ),
            {"organisation_id": organisation_id},
        )

    async def get_quota_usage(self, db: AsyncSession, organisation_id: str) -> dict[str, Any]:
        await self.ensure_rows(db, organisation_id)
        rows = await db.execute(
            text(
                """
                SELECT
                    q.max_agents, q.max_running_agents, q.max_cpu_millicores, q.max_memory_mib,
                    q.max_executions_day, q.max_executions_month, q.max_concurrent_exec,
                    q.max_log_retention_days, q.max_log_gb, q.max_api_rpm, q.max_schedules, q.max_k8s_pods,
                    u.current_agents, u.current_running, u.current_cpu_mc, u.current_memory_mib,
                    u.current_exec, u.executions_today, u.executions_month, u.api_requests_minute,
                    u.peak_agents, u.peak_running, u.period_start_day, u.period_start_month, u.updated_at
                FROM organisation_quotas q
                JOIN organisation_usage u ON u.organisation_id=q.organisation_id
                WHERE q.organisation_id=:organisation_id::uuid
                """
            ),
            {"organisation_id": organisation_id},
        )
        r = rows.fetchone()
        if not r:
            return {}
        return {
            "quota": {
                "max_agents": r[0],
                "max_running_agents": r[1],
                "max_cpu_millicores": r[2],
                "max_memory_mib": r[3],
                "max_executions_day": r[4],
                "max_executions_month": r[5],
                "max_concurrent_exec": r[6],
                "max_log_retention_days": r[7],
                "max_log_gb": float(r[8]),
                "max_api_rpm": r[9],
                "max_schedules": r[10],
                "max_k8s_pods": r[11],
            },
            "usage": {
                "current_agents": r[12],
                "current_running": r[13],
                "current_cpu_mc": r[14],
                "current_memory_mib": r[15],
                "current_exec": r[16],
                "executions_today": r[17],
                "executions_month": r[18],
                "api_requests_minute": r[19],
                "peak_agents": r[20],
                "peak_running": r[21],
                "period_start_day": r[22],
                "period_start_month": r[23],
                "updated_at": r[24],
            },
        }

    async def can_create_agent(
        self,
        db: AsyncSession,
        organisation_id: str,
        cpu_limit: float,
        memory_limit: str,
    ) -> dict[str, Any]:
        data = await self.get_quota_usage(db, organisation_id)
        if not data:
            return {"allowed": False, "reason": "quota_not_configured"}
        quota = data["quota"]
        usage = data["usage"]
        requested_cpu = int(float(cpu_limit or 0) * 1000)
        requested_mem = parse_memory_to_mib(memory_limit)

        if usage["current_agents"] + 1 > quota["max_agents"]:
            return {"allowed": False, "reason": "max_agents_exceeded"}
        if usage["current_cpu_mc"] + requested_cpu > quota["max_cpu_millicores"]:
            return {"allowed": False, "reason": "max_cpu_exceeded"}
        if usage["current_memory_mib"] + requested_mem > quota["max_memory_mib"]:
            return {"allowed": False, "reason": "max_memory_exceeded"}
        return {
            "allowed": True,
            "requested": {"cpu_mc": requested_cpu, "memory_mib": requested_mem},
            "remaining": {
                "agents": quota["max_agents"] - usage["current_agents"],
                "cpu_mc": quota["max_cpu_millicores"] - usage["current_cpu_mc"],
                "memory_mib": quota["max_memory_mib"] - usage["current_memory_mib"],
            },
        }

    async def can_start_execution(self, db: AsyncSession, organisation_id: str) -> dict[str, Any]:
        data = await self.get_quota_usage(db, organisation_id)
        if not data:
            return {"allowed": False, "reason": "quota_not_configured"}
        quota = data["quota"]
        usage = data["usage"]
        if usage["current_exec"] >= quota["max_concurrent_exec"]:
            return {"allowed": False, "reason": "max_concurrent_exec_exceeded"}
        if usage["executions_today"] >= quota["max_executions_day"]:
            return {"allowed": False, "reason": "max_executions_day_exceeded"}
        if usage["executions_month"] >= quota["max_executions_month"]:
            return {"allowed": False, "reason": "max_executions_month_exceeded"}
        return {"allowed": True}

    async def adjust_usage(
        self,
        db: AsyncSession,
        organisation_id: str,
        agents_delta: int = 0,
        running_delta: int = 0,
        cpu_mc_delta: int = 0,
        memory_mib_delta: int = 0,
        execution_delta: int = 0,
        executions_today_delta: int = 0,
        executions_month_delta: int = 0,
        api_requests_delta: int = 0,
    ) -> None:
        await self.ensure_rows(db, organisation_id)
        await db.execute(
            text(
                """
                UPDATE organisation_usage
                SET
                    current_agents = GREATEST(current_agents + :agents_delta, 0),
                    current_running = GREATEST(current_running + :running_delta, 0),
                    current_cpu_mc = GREATEST(current_cpu_mc + :cpu_mc_delta, 0),
                    current_memory_mib = GREATEST(current_memory_mib + :memory_mib_delta, 0),
                    current_exec = GREATEST(current_exec + :execution_delta, 0),
                    executions_today = GREATEST(executions_today + :executions_today_delta, 0),
                    executions_month = GREATEST(executions_month + :executions_month_delta, 0),
                    api_requests_minute = GREATEST(api_requests_minute + :api_requests_delta, 0),
                    peak_agents = GREATEST(peak_agents, current_agents + :agents_delta),
                    peak_running = GREATEST(peak_running, current_running + :running_delta),
                    updated_at = NOW()
                WHERE organisation_id=:organisation_id::uuid
                """
            ),
            {
                "organisation_id": organisation_id,
                "agents_delta": agents_delta,
                "running_delta": running_delta,
                "cpu_mc_delta": cpu_mc_delta,
                "memory_mib_delta": memory_mib_delta,
                "execution_delta": execution_delta,
                "executions_today_delta": executions_today_delta,
                "executions_month_delta": executions_month_delta,
                "api_requests_delta": api_requests_delta,
            },
        )


quota_enforcer = QuotaEnforcer()
