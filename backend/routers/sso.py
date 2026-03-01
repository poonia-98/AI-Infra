from typing import Any, Optional
from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncSession
from database import get_db
from services.sso_service import sso_service
from services.rate_limiter import rate_limiter
from security.auth import AuthenticatedUser, get_current_user


router = APIRouter(prefix="/api/v1/sso", tags=["sso"])


class OAuthLinkRequest(BaseModel):
    provider: str
    provider_user_id: str
    email: Optional[str] = None
    name: Optional[str] = None
    avatar_url: Optional[str] = None
    raw_profile: dict[str, Any] = Field(default_factory=dict)
    access_token: Optional[str] = None
    refresh_token: Optional[str] = None
    token_expires_at: Optional[str] = None


class SessionCreateRequest(BaseModel):
    user_agent: Optional[str] = None
    ip_address: Optional[str] = None
    device_id: Optional[str] = None


class RefreshRequest(BaseModel):
    refresh_token: str


class ValidateTokenRequest(BaseModel):
    token: str


async def sso_rate_limit(request: Request, db: AsyncSession = Depends(get_db)) -> None:
    client_ip = request.client.host if request.client else "unknown"
    key = f"sso:{client_ip}:{request.url.path}"
    result = await rate_limiter.allow(db, key=key, limit=30, window_seconds=60)
    if not result["allowed"]:
        raise HTTPException(status_code=429, detail="Too many requests")


@router.post("/oauth/link", status_code=201)
async def link_oauth_account(
    payload: OAuthLinkRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    account_id = await sso_service.link_oauth_account(
        db=db,
        user_id=user.user_id,
        provider=payload.provider,
        provider_user_id=payload.provider_user_id,
        email=payload.email,
        name=payload.name,
        avatar_url=payload.avatar_url,
        raw_profile=payload.raw_profile,
        access_token=payload.access_token,
        refresh_token=payload.refresh_token,
        token_expires_at=payload.token_expires_at,
    )
    return {"id": account_id}


@router.post("/sessions", status_code=201, dependencies=[Depends(sso_rate_limit)])
async def create_session(
    payload: SessionCreateRequest,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    tokens = await sso_service.create_session(
        db=db,
        user_id=user.user_id,
        organisation_id=user.organisation_id,
        role=user.role,
        user_agent=payload.user_agent,
        ip_address=payload.ip_address,
        device_id=payload.device_id,
    )
    return tokens


@router.post("/sessions/refresh")
async def refresh_session(payload: RefreshRequest, db: AsyncSession = Depends(get_db)):
    result = await sso_service.refresh_session(db, payload.refresh_token)
    if not result.get("success", True):
        raise HTTPException(status_code=401, detail=result.get("error"))
    return result


@router.post("/sessions/{session_id}/revoke")
async def revoke_session(session_id: str, reason: str = "manual_logout", db: AsyncSession = Depends(get_db)):
    ok = await sso_service.revoke_session(db, session_id, reason=reason)
    if not ok:
        raise HTTPException(status_code=404, detail="Session not found")
    return {"status": "revoked"}


@router.post("/token/validate", dependencies=[Depends(sso_rate_limit)])
async def validate_access_token(payload: ValidateTokenRequest, db: AsyncSession = Depends(get_db)):
    result = await sso_service.validate_access_token(db, payload.token)
    return {"valid": bool(result.get("valid"))}
