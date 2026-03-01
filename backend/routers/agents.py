import uuid
from typing import Optional
from fastapi import APIRouter, Depends, HTTPException, Query
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import select, desc
from database import get_db
from models import Agent, Execution, Log, Event, Metric
from schemas import (
    AgentCreate, AgentUpdate, AgentResponse, ExecutionResponse,
    LogResponse, EventResponse, MetricResponse, AgentAction
)
from services.agent_service import agent_service
from security.auth import AuthenticatedUser, get_current_user

router = APIRouter(prefix="/agents", tags=["agents"])


@router.get("", response_model=list[AgentResponse])
async def list_agents(
    skip: int = Query(0, ge=0),
    limit: int = Query(100, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await agent_service.list_agents(db, organisation_id=user.organisation_id, skip=skip, limit=limit)


@router.post("", response_model=AgentResponse, status_code=201)
async def create_agent(
    data: AgentCreate,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await agent_service.create_agent(db, data, organisation_id=user.organisation_id)


@router.get("/{agent_id}", response_model=AgentResponse)
async def get_agent(
    agent_id: uuid.UUID,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    agent = await agent_service.get_agent(db, agent_id, organisation_id=user.organisation_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")
    return agent


@router.put("/{agent_id}", response_model=AgentResponse)
async def update_agent(
    agent_id: uuid.UUID,
    data: AgentUpdate,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    agent = await agent_service.update_agent(db, agent_id, data, organisation_id=user.organisation_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")
    return agent


@router.delete("/{agent_id}", status_code=204)
async def delete_agent(
    agent_id: uuid.UUID,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    deleted = await agent_service.delete_agent(db, agent_id, organisation_id=user.organisation_id)
    if not deleted:
        raise HTTPException(status_code=404, detail="Agent not found")


@router.post("/{agent_id}/start")
async def start_agent(
    agent_id: uuid.UUID,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    result = await agent_service.start_agent(db, agent_id, organisation_id=user.organisation_id)
    if not result.get("success"):
        raise HTTPException(status_code=400, detail=result.get("error", "Failed to start agent"))
    return result


@router.post("/{agent_id}/stop")
async def stop_agent(
    agent_id: uuid.UUID,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    result = await agent_service.stop_agent(db, agent_id, organisation_id=user.organisation_id)
    if not result.get("success"):
        raise HTTPException(status_code=400, detail=result.get("error", "Failed to stop agent"))
    return result


@router.post("/{agent_id}/restart")
async def restart_agent(
    agent_id: uuid.UUID,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    result = await agent_service.restart_agent(db, agent_id, organisation_id=user.organisation_id)
    if not result.get("success"):
        raise HTTPException(status_code=400, detail=result.get("error", "Failed to restart agent"))
    return result


@router.get("/{agent_id}/executions", response_model=list[ExecutionResponse])
async def get_agent_executions(
    agent_id: uuid.UUID,
    skip: int = Query(0, ge=0),
    limit: int = Query(50, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    agent = await agent_service.get_agent(db, agent_id, organisation_id=user.organisation_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")
    result = await db.execute(
        select(Execution)
        .where(Execution.agent_id == agent_id)
        .order_by(desc(Execution.created_at))
        .offset(skip)
        .limit(limit)
    )
    return list(result.scalars().all())


@router.get("/{agent_id}/logs", response_model=list[LogResponse])
async def get_agent_logs(
    agent_id: uuid.UUID,
    skip: int = Query(0, ge=0),
    limit: int = Query(200, ge=1, le=2000),
    level: Optional[str] = None,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    agent = await agent_service.get_agent(db, agent_id, organisation_id=user.organisation_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")
    query = select(Log).where(Log.agent_id == agent_id).order_by(desc(Log.timestamp))
    if level:
        query = query.where(Log.level == level)
    query = query.offset(skip).limit(limit)
    result = await db.execute(query)
    return list(result.scalars().all())


@router.get("/{agent_id}/events", response_model=list[EventResponse])
async def get_agent_events(
    agent_id: uuid.UUID,
    skip: int = Query(0, ge=0),
    limit: int = Query(100, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    agent = await agent_service.get_agent(db, agent_id, organisation_id=user.organisation_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")
    result = await db.execute(
        select(Event)
        .where(Event.agent_id == agent_id)
        .order_by(desc(Event.created_at))
        .offset(skip)
        .limit(limit)
    )
    return list(result.scalars().all())


@router.get("/{agent_id}/metrics", response_model=list[MetricResponse])
async def get_agent_metrics(
    agent_id: uuid.UUID,
    metric_name: Optional[str] = None,
    limit: int = Query(100, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    agent = await agent_service.get_agent(db, agent_id, organisation_id=user.organisation_id)
    if not agent:
        raise HTTPException(status_code=404, detail="Agent not found")
    query = select(Metric).where(Metric.agent_id == agent_id).order_by(desc(Metric.timestamp))
    if metric_name:
        query = query.where(Metric.metric_name == metric_name)
    query = query.limit(limit)
    result = await db.execute(query)
    return list(result.scalars().all())
