from typing import Any, Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncSession

from database import get_db
from services.webhook_service import webhook_service
from security.auth import AuthenticatedUser, get_current_user


router = APIRouter(prefix="/api/v1/webhooks", tags=["webhooks"])


class WebhookCreate(BaseModel):
    name: str
    endpoint: str
    events: list[str] = Field(default_factory=list)
    agent_id: Optional[str] = None
    workflow_id: Optional[str] = None
    secret: Optional[str] = None


class DispatchRequest(BaseModel):
    event_type: str
    payload: dict[str, Any] = Field(default_factory=dict)
    event_id: Optional[str] = None


@router.post("", status_code=201)
async def create_webhook(
    payload: WebhookCreate,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    webhook_id = await webhook_service.create_webhook(
        db=db,
        organisation_id=user.organisation_id,
        name=payload.name,
        endpoint=payload.endpoint,
        events=payload.events,
        agent_id=payload.agent_id,
        workflow_id=payload.workflow_id,
        secret=payload.secret,
    )
    return {"id": webhook_id}


@router.get("")
async def list_webhooks(
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await webhook_service.list_webhooks(db, organisation_id=user.organisation_id)


@router.post("/{webhook_id}/disable")
async def disable_webhook(webhook_id: str, db: AsyncSession = Depends(get_db)):
    ok = await webhook_service.disable_webhook(db, webhook_id)
    if not ok:
        raise HTTPException(status_code=404, detail="Webhook not found")
    return {"status": "disabled"}


@router.get("/deliveries")
async def list_deliveries(
    webhook_id: Optional[str] = None,
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
):
    return await webhook_service.list_deliveries(db, webhook_id=webhook_id, limit=limit)


@router.post("/dispatch")
async def dispatch(
    payload: DispatchRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    count = await webhook_service.dispatch_event(
        db=db,
        organisation_id=user.organisation_id,
        event_type=payload.event_type,
        payload=payload.payload,
        event_id=payload.event_id,
    )
    return {"dispatched": count}


@router.post("/retry")
async def retry_pending(
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
):
    count = await webhook_service.retry_pending(db, limit=limit)
    return {"scheduled": count}
