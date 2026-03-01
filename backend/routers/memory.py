from typing import Any, Optional
from fastapi import APIRouter, Depends
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncSession
from database import get_db
from services.memory_service import memory_service
from security.auth import AuthenticatedUser, get_current_user


router = APIRouter(prefix="/api/v1/memory", tags=["memory"])


class MemoryStoreRequest(BaseModel):
    agent_id: str
    content: str
    memory_type: str = "short_term"
    key: Optional[str] = None
    metadata: dict[str, Any] = Field(default_factory=dict)
    importance: float = 0.5
    embedding: Optional[list[float]] = None
    session_id: Optional[str] = None
    ttl_seconds: Optional[int] = None


class MemoryRecallRequest(BaseModel):
    agent_id: str
    memory_type: Optional[str] = None
    key: Optional[str] = None
    session_id: Optional[str] = None
    limit: int = 50
    include_expired: bool = False


class MemorySearchRequest(BaseModel):
    agent_id: str
    embedding: list[float]
    memory_type: Optional[str] = None
    limit: int = 10
    threshold: float = 0.7


class MemoryForgetRequest(BaseModel):
    agent_id: str
    memory_id: Optional[str] = None
    key: Optional[str] = None
    memory_type: Optional[str] = None
    session_id: Optional[str] = None


@router.post("", status_code=201)
async def store_memory(
    payload: MemoryStoreRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    memory_id = await memory_service.store(
        db=db,
        agent_id=payload.agent_id,
        organisation_id=user.organisation_id,
        content=payload.content,
        memory_type=payload.memory_type,
        key=payload.key,
        metadata=payload.metadata,
        importance=payload.importance,
        embedding=payload.embedding,
        session_id=payload.session_id,
        ttl_seconds=payload.ttl_seconds,
    )
    return {"id": memory_id}


@router.post("/recall")
async def recall_memory(
    payload: MemoryRecallRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await memory_service.recall(
        db=db,
        agent_id=payload.agent_id,
        organisation_id=user.organisation_id,
        memory_type=payload.memory_type,
        key=payload.key,
        session_id=payload.session_id,
        limit=payload.limit,
        include_expired=payload.include_expired,
    )


@router.post("/search")
async def search_memory(
    payload: MemorySearchRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await memory_service.search_similar(
        db=db,
        agent_id=payload.agent_id,
        organisation_id=user.organisation_id,
        embedding=payload.embedding,
        memory_type=payload.memory_type,
        limit=payload.limit,
        threshold=payload.threshold,
    )


@router.delete("")
async def forget_memory(
    payload: MemoryForgetRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    deleted = await memory_service.forget(
        db=db,
        agent_id=payload.agent_id,
        organisation_id=user.organisation_id,
        memory_id=payload.memory_id,
        key=payload.key,
        memory_type=payload.memory_type,
        session_id=payload.session_id,
    )
    return {"deleted": deleted}


@router.post("/consolidate")
async def consolidate_memory(
    agent_id: str,
    session_id: str,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    promoted = await memory_service.consolidate(
        db=db,
        agent_id=agent_id,
        organisation_id=user.organisation_id,
        session_id=session_id,
    )
    return {"promoted": promoted}


@router.post("/expire")
async def expire_memory(db: AsyncSession = Depends(get_db)):
    removed = await memory_service.expire_short_term(db)
    return {"removed": removed}
