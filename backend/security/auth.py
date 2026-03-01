from dataclasses import dataclass
from datetime import datetime, timezone
import hmac

from fastapi import Depends, HTTPException, Request, status
from fastapi.security import HTTPAuthorizationCredentials, HTTPBearer
from jose import JWTError, jwt
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from config import settings
from database import get_db

bearer = HTTPBearer(auto_error=False)


@dataclass(frozen=True)
class AuthenticatedUser:
    user_id: str
    organisation_id: str
    role: str
    session_id: str


async def get_current_user(
    request: Request,
    credentials: HTTPAuthorizationCredentials = Depends(bearer),
    db: AsyncSession = Depends(get_db),
) -> AuthenticatedUser:
    service_key = request.headers.get("X-Service-API-Key")
    if service_key and settings.CONTROL_PLANE_API_KEY and hmac.compare_digest(service_key, settings.CONTROL_PLANE_API_KEY):
        user = AuthenticatedUser(
            user_id="system",
            organisation_id=request.headers.get("X-Organisation-ID", ""),
            role="admin",
            session_id="service",
        )
        request.state.user = user
        return user

    if credentials is None or credentials.scheme.lower() != "bearer":
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Missing bearer token")
    try:
        payload = jwt.decode(credentials.credentials, settings.SECRET_KEY, algorithms=[settings.JWT_ALGORITHM])
    except JWTError:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Invalid token")
    if payload.get("type") != "access":
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Invalid token type")

    user_id = payload.get("sub")
    organisation_id = payload.get("org_id")
    role = payload.get("role")
    session_id = payload.get("sid")
    if not user_id or not organisation_id or not role or not session_id:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Missing token claims")

    row = await db.execute(
        text(
            """
            SELECT revoked, access_expires_at
            FROM sessions
            WHERE id=:session_id::uuid
            """
        ),
        {"session_id": session_id},
    )
    session = row.fetchone()
    now = datetime.now(timezone.utc)
    if not session or session[0] or (session[1] and session[1] < now):
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="Session invalid")

    user = AuthenticatedUser(
        user_id=user_id,
        organisation_id=organisation_id,
        role=role,
        session_id=session_id,
    )
    request.state.user = user
    return user
