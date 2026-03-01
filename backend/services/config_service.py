import base64
import hashlib
import json
import os
from datetime import datetime
from typing import Any, Optional

from cryptography.fernet import Fernet, InvalidToken, MultiFernet
from cachetools import TTLCache
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession


def _derive_fernet_key(passphrase: str) -> bytes:
    digest = hashlib.sha256(passphrase.encode()).digest()
    return base64.urlsafe_b64encode(digest)


def _get_fernet() -> MultiFernet:
    primary = os.getenv("SECRETS_PASSPHRASE", "")
    if len(primary) < 32:
        raise RuntimeError("SECRETS_PASSPHRASE must be set and at least 32 characters")
    ferns: list[Fernet] = [Fernet(_derive_fernet_key(primary))]
    previous_raw = os.getenv("SECRETS_PASSPHRASE_PREVIOUS", "")
    for item in previous_raw.split(","):
        item = item.strip()
        if item:
            ferns.append(Fernet(_derive_fernet_key(item)))
    return MultiFernet(ferns)


class ConfigService:
    _cache: TTLCache = TTLCache(maxsize=2048, ttl=300)

    @classmethod
    def _key(cls, service: str, key: str) -> str:
        return f"{service}:{key}"

    @classmethod
    async def get(cls, db: AsyncSession, service: str, key: str) -> Optional[str]:
        cache_key = cls._key(service, key)
        if cache_key in cls._cache:
            return cls._cache[cache_key]
        rows = await db.execute(
            text(
                """
                SELECT value
                FROM configs
                WHERE service=:service AND key=:key
                """
            ),
            {"service": service, "key": key},
        )
        row = rows.fetchone()
        if not row:
            return None
        cls._cache[cache_key] = row[0]
        return row[0]

    @classmethod
    async def set(
        cls,
        db: AsyncSession,
        service: str,
        key: str,
        value: str,
        value_type: str = "string",
        description: Optional[str] = None,
        changed_by: str = "system",
    ) -> None:
        old_rows = await db.execute(
            text("SELECT id::text, value, version FROM configs WHERE service=:service AND key=:key"),
            {"service": service, "key": key},
        )
        old = old_rows.fetchone()
        if old:
            await db.execute(
                text(
                    """
                    UPDATE configs
                    SET value=:value, value_type=:value_type, description=:description,
                        version=version+1, updated_at=NOW()
                    WHERE id=:id::uuid
                    """
                ),
                {
                    "id": old[0],
                    "value": value,
                    "value_type": value_type,
                    "description": description,
                },
            )
            await db.execute(
                text(
                    """
                    INSERT INTO config_history
                      (config_id, old_value, new_value, changed_by, version, changed_at)
                    VALUES
                      (:config_id::uuid, :old_value, :new_value, :changed_by, :version, NOW())
                    """
                ),
                {
                    "config_id": old[0],
                    "old_value": old[1],
                    "new_value": value,
                    "changed_by": changed_by,
                    "version": int(old[2]) + 1,
                },
            )
        else:
            await db.execute(
                text(
                    """
                    INSERT INTO configs
                      (service, key, value, value_type, description, version, updated_at)
                    VALUES
                      (:service, :key, :value, :value_type, :description, 1, NOW())
                    """
                ),
                {
                    "service": service,
                    "key": key,
                    "value": value,
                    "value_type": value_type,
                    "description": description,
                },
            )
        cls._cache[cls._key(service, key)] = value

    @classmethod
    def invalidate(cls, service: str, key: str) -> None:
        cls._cache.pop(cls._key(service, key), None)


class SecretsService:
    @staticmethod
    async def set(
        db: AsyncSession,
        service: str,
        name: str,
        value: str,
        description: Optional[str] = None,
        expires_at: Optional[datetime] = None,
        created_by: str = "system",
    ) -> None:
        encrypted_value = _get_fernet().encrypt(value.encode())
        await db.execute(
            text(
                """
                INSERT INTO secrets
                  (service, name, encrypted_value, description, expires_at, created_by, active)
                VALUES
                  (:service, :name, :value, :description, :expires_at, :created_by, TRUE)
                ON CONFLICT (service, name) DO UPDATE SET
                  encrypted_value=EXCLUDED.encrypted_value,
                  description=EXCLUDED.description,
                  expires_at=EXCLUDED.expires_at,
                  active=TRUE,
                  updated_at=NOW()
                """
            ),
            {
                "service": service,
                "name": name,
                "value": encrypted_value,
                "description": description,
                "expires_at": expires_at,
                "created_by": created_by,
            },
        )

    @staticmethod
    async def get(db: AsyncSession, service: str, name: str) -> Optional[str]:
        rows = await db.execute(
            text(
                """
                SELECT encrypted_value
                FROM secrets
                WHERE service=:service
                  AND name=:name
                  AND active=TRUE
                  AND (expires_at IS NULL OR expires_at > NOW())
                """
            ),
            {"service": service, "name": name},
        )
        row = rows.fetchone()
        if not row:
            return None
        try:
            return _get_fernet().decrypt(bytes(row[0])).decode()
        except InvalidToken:
            return None

    @staticmethod
    async def list_names(db: AsyncSession, service: str) -> list[str]:
        rows = await db.execute(
            text(
                """
                SELECT name
                FROM secrets
                WHERE service=:service
                  AND active=TRUE
                  AND (expires_at IS NULL OR expires_at > NOW())
                ORDER BY name
                """
            ),
            {"service": service},
        )
        return [r[0] for r in rows.fetchall()]

    @staticmethod
    async def delete(db: AsyncSession, service: str, name: str) -> bool:
        result = await db.execute(
            text(
                """
                UPDATE secrets
                SET active=FALSE, updated_at=NOW()
                WHERE service=:service AND name=:name AND active=TRUE
                """
            ),
            {"service": service, "name": name},
        )
        return result.rowcount > 0

