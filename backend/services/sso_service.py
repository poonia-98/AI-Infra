import hashlib
import json
import uuid
from datetime import datetime, timedelta, timezone
from typing import Any, Optional
from jose import jwt, JWTError
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from config import settings
from services.config_service import _get_fernet


class SSOService:
    def __init__(self):
        self.secret = settings.SECRET_KEY
        self.algorithm = settings.JWT_ALGORITHM
        self.access_ttl_minutes = 60
        self.refresh_ttl_days = 30

    def _now(self) -> datetime:
        return datetime.now(timezone.utc)

    def _hash_token(self, value: str) -> str:
        return hashlib.sha256(value.encode()).hexdigest()

    def _encrypt_token(self, value: Optional[str]) -> Optional[str]:
        if not value:
            return None
        return _get_fernet().encrypt(value.encode()).decode()

    def _decrypt_token(self, value: Optional[str]) -> Optional[str]:
        if not value:
            return None
        return _get_fernet().decrypt(value.encode()).decode()

    def _build_token(self, payload: dict[str, Any], expires_delta: timedelta) -> tuple[str, datetime]:
        exp = self._now() + expires_delta
        claims = dict(payload)
        claims["exp"] = int(exp.timestamp())
        token = jwt.encode(claims, self.secret, algorithm=self.algorithm)
        return token, exp

    async def link_oauth_account(
        self,
        db: AsyncSession,
        user_id: str,
        provider: str,
        provider_user_id: str,
        email: Optional[str] = None,
        name: Optional[str] = None,
        avatar_url: Optional[str] = None,
        raw_profile: Optional[dict[str, Any]] = None,
        access_token: Optional[str] = None,
        refresh_token: Optional[str] = None,
        token_expires_at: Optional[str] = None,
    ) -> str:
        result = await db.execute(
            text(
                """
                INSERT INTO oauth_accounts
                  (user_id, provider, provider_user_id, email, name, avatar_url,
                   raw_profile, access_token, refresh_token, token_expires_at)
                VALUES
                  (:user_id::uuid, :provider, :provider_user_id, :email, :name, :avatar_url,
                   :raw_profile::jsonb, :access_token, :refresh_token, :token_expires_at::timestamptz)
                ON CONFLICT (provider, provider_user_id) DO UPDATE SET
                  email=EXCLUDED.email,
                  name=EXCLUDED.name,
                  avatar_url=EXCLUDED.avatar_url,
                  raw_profile=EXCLUDED.raw_profile,
                  access_token=EXCLUDED.access_token,
                  refresh_token=EXCLUDED.refresh_token,
                  token_expires_at=EXCLUDED.token_expires_at,
                  updated_at=NOW()
                RETURNING id::text
                """
            ),
            {
                "user_id": user_id,
                "provider": provider,
                "provider_user_id": provider_user_id,
                "email": email,
                "name": name,
                "avatar_url": avatar_url,
                "raw_profile": json.dumps(raw_profile or {}),
                "access_token": self._encrypt_token(access_token),
                "refresh_token": self._encrypt_token(refresh_token),
                "token_expires_at": token_expires_at,
            },
        )
        row = result.fetchone()
        return row[0] if row else ""

    async def create_session(
        self,
        db: AsyncSession,
        user_id: str,
        organisation_id: Optional[str] = None,
        role: str = "developer",
        user_agent: Optional[str] = None,
        ip_address: Optional[str] = None,
        device_id: Optional[str] = None,
    ) -> dict[str, Any]:
        session_id = str(uuid.uuid4())
        access_jti = str(uuid.uuid4())
        refresh_jti = str(uuid.uuid4())

        access_payload = {
            "sub": user_id,
            "org_id": organisation_id,
            "role": role,
            "sid": session_id,
            "jti": access_jti,
            "type": "access",
        }
        refresh_payload = {
            "sub": user_id,
            "sid": session_id,
            "jti": refresh_jti,
            "type": "refresh",
        }
        access_token, access_exp = self._build_token(access_payload, timedelta(minutes=self.access_ttl_minutes))
        refresh_token, refresh_exp = self._build_token(refresh_payload, timedelta(days=self.refresh_ttl_days))

        await db.execute(
            text(
                """
                INSERT INTO sessions
                  (id, user_id, organisation_id, access_jti, refresh_jti, refresh_token_hash,
                   access_expires_at, refresh_expires_at, user_agent, ip_address, device_id, last_used_at)
                VALUES
                  (:id::uuid, :user_id::uuid, :organisation_id::uuid, :access_jti, :refresh_jti, :refresh_hash,
                   :access_expires_at, :refresh_expires_at, :user_agent, :ip_address::inet, :device_id, NOW())
                """
            ),
            {
                "id": session_id,
                "user_id": user_id,
                "organisation_id": organisation_id,
                "access_jti": access_jti,
                "refresh_jti": refresh_jti,
                "refresh_hash": self._hash_token(refresh_token),
                "access_expires_at": access_exp,
                "refresh_expires_at": refresh_exp,
                "user_agent": user_agent,
                "ip_address": ip_address,
                "device_id": device_id,
            },
        )

        return {
            "session_id": session_id,
            "access_token": access_token,
            "refresh_token": refresh_token,
            "access_expires_at": access_exp,
            "refresh_expires_at": refresh_exp,
            "token_type": "bearer",
        }

    async def refresh_session(self, db: AsyncSession, refresh_token: str) -> dict[str, Any]:
        try:
            payload = jwt.decode(refresh_token, self.secret, algorithms=[self.algorithm])
        except JWTError:
            return {"success": False, "error": "invalid_refresh_token"}

        if payload.get("type") != "refresh":
            return {"success": False, "error": "invalid_token_type"}

        session_id = payload.get("sid")
        user_id = payload.get("sub")
        refresh_jti = payload.get("jti")
        if not session_id or not user_id or not refresh_jti:
            return {"success": False, "error": "invalid_token_claims"}

        rows = await db.execute(
            text(
                """
                SELECT id::text, user_id::text, organisation_id::text, revoked, refresh_jti, refresh_token_hash, refresh_expires_at
                FROM sessions
                WHERE id=:session_id::uuid
                """
            ),
            {"session_id": session_id},
        )
        session = rows.fetchone()
        if not session:
            return {"success": False, "error": "session_not_found"}
        if session[3]:
            return {"success": False, "error": "session_revoked"}
        if session[4] != refresh_jti:
            return {"success": False, "error": "refresh_jti_mismatch"}
        if self._hash_token(refresh_token) != session[5]:
            return {"success": False, "error": "refresh_token_hash_mismatch"}
        if session[6] and session[6] < self._now():
            return {"success": False, "error": "refresh_token_expired"}

        access_jti = str(uuid.uuid4())
        new_refresh_jti = str(uuid.uuid4())
        access_payload = {
            "sub": session[1],
            "org_id": session[2],
            "sid": session[0],
            "jti": access_jti,
            "type": "access",
        }
        refresh_payload = {
            "sub": session[1],
            "sid": session[0],
            "jti": new_refresh_jti,
            "type": "refresh",
        }
        access_token, access_exp = self._build_token(access_payload, timedelta(minutes=self.access_ttl_minutes))
        new_refresh_token, refresh_exp = self._build_token(refresh_payload, timedelta(days=self.refresh_ttl_days))

        await db.execute(
            text(
                """
                UPDATE sessions
                SET access_jti=:access_jti,
                    refresh_jti=:refresh_jti,
                    refresh_token_hash=:refresh_hash,
                    access_expires_at=:access_exp,
                    refresh_expires_at=:refresh_exp,
                    last_used_at=NOW()
                WHERE id=:session_id::uuid
                """
            ),
            {
                "session_id": session[0],
                "access_jti": access_jti,
                "refresh_jti": new_refresh_jti,
                "refresh_hash": self._hash_token(new_refresh_token),
                "access_exp": access_exp,
                "refresh_exp": refresh_exp,
            },
        )
        return {
            "success": True,
            "session_id": session[0],
            "access_token": access_token,
            "refresh_token": new_refresh_token,
            "access_expires_at": access_exp,
            "refresh_expires_at": refresh_exp,
            "token_type": "bearer",
        }

    async def revoke_session(self, db: AsyncSession, session_id: str, reason: str = "manual_logout") -> bool:
        result = await db.execute(
            text(
                """
                UPDATE sessions
                SET revoked=TRUE, revoked_at=NOW(), revoke_reason=:reason
                WHERE id=:session_id::uuid AND revoked=FALSE
                """
            ),
            {"session_id": session_id, "reason": reason},
        )
        return result.rowcount > 0

    async def validate_access_token(self, db: AsyncSession, token: str) -> dict[str, Any]:
        try:
            payload = jwt.decode(token, self.secret, algorithms=[self.algorithm])
        except JWTError:
            return {"valid": False}
        if payload.get("type") != "access":
            return {"valid": False}
        session_id = payload.get("sid")
        if not session_id:
            return {"valid": False}
        rows = await db.execute(
            text(
                """
                SELECT revoked, access_expires_at
                FROM sessions
                WHERE id=:session_id::uuid
                """
            ),
            {"session_id": session_id},
        )
        session = rows.fetchone()
        if not session:
            return {"valid": False}
        if session[0]:
            return {"valid": False}
        if session[1] and session[1] < self._now():
            return {"valid": False}
        return {"valid": True}


sso_service = SSOService()
