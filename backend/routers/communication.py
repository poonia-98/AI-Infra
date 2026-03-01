from typing import Any, Optional
from fastapi import APIRouter, Depends, Query
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncSession
from database import get_db
from services.communication_service import communication_service
from security.auth import AuthenticatedUser, get_current_user


router = APIRouter(prefix="/api/v1/communication", tags=["communication"])


class SendMessageRequest(BaseModel):
    from_agent_id: str
    to_agent_id: Optional[str] = None
    to_group: Optional[str] = None
    message_type: str = "task"
    subject: Optional[str] = None
    payload: dict[str, Any] = Field(default_factory=dict)
    correlation_id: Optional[str] = None
    reply_to: Optional[str] = None
    priority: int = 5
    expires_at: Optional[str] = None


class RegisterSpawnRequest(BaseModel):
    parent_agent_id: str
    child_agent_id: str
    spawn_reason: Optional[str] = None
    spawn_config: dict[str, Any] = Field(default_factory=dict)


@router.post("/messages", status_code=201)
async def send_message(
    payload: SendMessageRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    message_id = await communication_service.send_message(
        db=db,
        organisation_id=user.organisation_id,
        from_agent_id=payload.from_agent_id,
        to_agent_id=payload.to_agent_id,
        to_group=payload.to_group,
        message_type=payload.message_type,
        subject=payload.subject,
        payload=payload.payload,
        correlation_id=payload.correlation_id,
        reply_to=payload.reply_to,
        priority=payload.priority,
        expires_at=payload.expires_at,
    )
    return {"id": message_id}


@router.get("/agents/{agent_id}/inbox")
async def inbox(
    agent_id: str,
    status: str = "pending",
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await communication_service.get_inbox(
        db=db,
        agent_id=agent_id,
        organisation_id=user.organisation_id,
        status=status,
        limit=limit,
    )


@router.post("/messages/{message_id}/read")
async def mark_message_read(
    message_id: str,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    ok = await communication_service.mark_read(db, message_id, organisation_id=user.organisation_id)
    return {"status": "ok" if ok else "not_found"}


@router.post("/spawn-tree", status_code=201)
async def register_spawn(
    payload: RegisterSpawnRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    record_id = await communication_service.register_spawn(
        db=db,
        organisation_id=user.organisation_id,
        parent_agent_id=payload.parent_agent_id,
        child_agent_id=payload.child_agent_id,
        spawn_reason=payload.spawn_reason,
        spawn_config=payload.spawn_config,
    )
    return {"id": record_id}


@router.get("/agents/{agent_id}/children")
async def get_children(
    agent_id: str,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await communication_service.get_children(
        db,
        parent_agent_id=agent_id,
        organisation_id=user.organisation_id,
    )


@router.get("/agents/discover")
async def discover_agents(
    group: Optional[str] = None,
    status: str = "running",
    limit: int = Query(200, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await communication_service.discover_agents(
        db,
        organisation_id=user.organisation_id,
        group=group,
        status=status,
        limit=limit,
    )
