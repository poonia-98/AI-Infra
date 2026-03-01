from typing import Optional
from fastapi import APIRouter, Depends, HTTPException
from pydantic import BaseModel
from sqlalchemy.ext.asyncio import AsyncSession
from database import get_db
from services.quota_enforcer import quota_enforcer
from services.rate_limiter import rate_limiter


router = APIRouter(prefix="/api/v1/quotas", tags=["quotas"])


class AgentQuotaCheckRequest(BaseModel):
    cpu_limit: float = 1.0
    memory_limit: str = "512m"


class UsageAdjustRequest(BaseModel):
    agents_delta: int = 0
    running_delta: int = 0
    cpu_mc_delta: int = 0
    memory_mib_delta: int = 0
    execution_delta: int = 0
    executions_today_delta: int = 0
    executions_month_delta: int = 0
    api_requests_delta: int = 0


@router.get("/{organisation_id}")
async def get_quotas(organisation_id: str, db: AsyncSession = Depends(get_db)):
    data = await quota_enforcer.get_quota_usage(db, organisation_id)
    if not data:
        raise HTTPException(status_code=404, detail="Quota record not found")
    return data


@router.post("/{organisation_id}/check-agent")
async def check_agent_quota(
    organisation_id: str,
    payload: AgentQuotaCheckRequest,
    db: AsyncSession = Depends(get_db),
):
    return await quota_enforcer.can_create_agent(
        db=db,
        organisation_id=organisation_id,
        cpu_limit=payload.cpu_limit,
        memory_limit=payload.memory_limit,
    )


@router.post("/{organisation_id}/check-execution")
async def check_execution_quota(organisation_id: str, db: AsyncSession = Depends(get_db)):
    return await quota_enforcer.can_start_execution(db, organisation_id)


@router.post("/{organisation_id}/usage")
async def adjust_usage(
    organisation_id: str,
    payload: UsageAdjustRequest,
    db: AsyncSession = Depends(get_db),
):
    await quota_enforcer.adjust_usage(
        db=db,
        organisation_id=organisation_id,
        agents_delta=payload.agents_delta,
        running_delta=payload.running_delta,
        cpu_mc_delta=payload.cpu_mc_delta,
        memory_mib_delta=payload.memory_mib_delta,
        execution_delta=payload.execution_delta,
        executions_today_delta=payload.executions_today_delta,
        executions_month_delta=payload.executions_month_delta,
        api_requests_delta=payload.api_requests_delta,
    )
    return {"status": "ok"}


@router.get("/{organisation_id}/rate-limit")
async def check_rate_limit(
    organisation_id: str,
    key: Optional[str] = None,
    limit: int = 1000,
    window_seconds: int = 60,
    db: AsyncSession = Depends(get_db),
):
    effective_key = key or f"org:{organisation_id}:api"
    return await rate_limiter.allow(db, effective_key, limit, window_seconds)
