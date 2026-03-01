"""
Centralised Config and Secrets management.

Config: reads from configs table with environment variable override.
  Override convention: CONFIG_<SERVICE>_<KEY_UPPER_SNAKE>
  e.g.  CONFIG_SCHEDULER_TICK_INTERVAL_SECONDS=5

Secrets: encrypted storage using Fernet symmetric encryption.
  Encryption key is the SECRETS_PASSPHRASE env var.
  Table stores ciphertext; plaintext is never persisted.
"""
import os
import json
import base64
import hashlib
from typing import Optional, Any
from datetime import datetime, timezone

from cryptography.fernet import Fernet, InvalidToken
from cachetools import TTLCache
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text, select
import structlog

logger = structlog.get_logger()

# ── Fernet key derivation ─────────────────────────────────────────────────────

def _derive_fernet_key(passphrase: str) -> bytes:
    """Derive a valid Fernet key from an arbitrary passphrase using SHA-256."""
    digest = hashlib.sha256(passphrase.encode()).digest()
    return base64.urlsafe_b64encode(digest)


def _get_fernet() -> Fernet:
    passphrase = os.getenv("SECRETS_PASSPHRASE", "change-me-in-production-SECRETS_PASSPHRASE")
    key = _derive_fernet_key(passphrase)
    return Fernet(key)


# ── Config Service ────────────────────────────────────────────────────────────

class ConfigService:
    """
    Read/write platform configuration stored in the `configs` table.
    Environment variables always override database values.
    Results are cached per request; call invalidate() to flush.
    """

    _cache: TTLCache = TTLCache(maxsize=2048, ttl=300)

    @staticmethod
    def _env_key(service: str, key: str) -> str:
        return f"CONFIG_{service.upper().replace('-', '_').replace('.', '_')}_{key.upper().replace('-', '_').replace('.', '_')}"

    @classmethod
    async def get(
        cls,
        db: AsyncSession,
        service: str,
        key: str,
        default: Any = None,
    ) -> Any:
        """Return config value. Env override takes precedence over DB."""
        env_val = os.getenv(cls._env_key(service, key))
        if env_val is not None:
            return env_val

        cache_key = f"{service}:{key}"
        if cache_key in cls._cache:
            return cls._cache[cache_key]

        # Try service-scoped config first, then global
        for svc in (service, "global"):
            row = await db.execute(
                text("SELECT value, value_type FROM configs WHERE service=:s AND key=:k LIMIT 1"),
                {"s": svc, "k": key},
            )
            rec = row.fetchone()
            if rec:
                value = cls._cast(rec[0], rec[1])
                cls._cache[cache_key] = value
                return value

        return default

    @classmethod
    async def set(
        cls,
        db: AsyncSession,
        service: str,
        key: str,
        value: Any,
        value_type: str = "string",
        description: Optional[str] = None,
        changed_by: str = "system",
    ) -> None:
        """Upsert a config value and record the change in config_history."""
        str_value = json.dumps(value) if value_type == "json" else str(value)

        # Fetch current version for history
        row = await db.execute(
            text("SELECT id, value, version FROM configs WHERE service=:s AND key=:k"),
            {"s": service, "k": key},
        )
        existing = row.fetchone()

        if existing:
            config_id, old_value, version = existing
            await db.execute(text("""
                UPDATE configs
                SET value=:v, version=version+1, updated_at=NOW()
                WHERE id=:id
            """), {"v": str_value, "id": config_id})

            await db.execute(text("""
                INSERT INTO config_history
                  (config_id, service, key, old_value, new_value, version, changed_by)
                VALUES (:cid::uuid, :s, :k, :old, :new, :ver, :by)
            """), {
                "cid": str(config_id), "s": service, "k": key,
                "old": old_value, "new": str_value, "ver": version + 1,
                "by": changed_by,
            })
        else:
            await db.execute(text("""
                INSERT INTO configs (service, key, value, value_type, description, created_by)
                VALUES (:s, :k, :v, :t, :d, :by)
                ON CONFLICT (service, key) DO UPDATE
                  SET value=EXCLUDED.value, version=configs.version+1, updated_at=NOW()
            """), {
                "s": service, "k": key, "v": str_value,
                "t": value_type, "d": description, "by": changed_by,
            })

        # Invalidate cache
        cls._cache.pop(f"{service}:{key}", None)
        cls._cache.pop(f"global:{key}", None)

    @staticmethod
    def _cast(value: str, value_type: str) -> Any:
        if value_type == "int":
            try:
                return int(value)
            except (ValueError, TypeError):
                return value
        if value_type == "bool":
            return value.lower() in ("true", "1", "yes")
        if value_type == "json":
            try:
                return json.loads(value)
            except Exception:
                return value
        return value

    @classmethod
    def invalidate(cls, service: Optional[str] = None, key: Optional[str] = None):
        if service and key:
            cls._cache.pop(f"{service}:{key}", None)
        else:
            cls._cache.clear()


# ── Secrets Service ───────────────────────────────────────────────────────────

class SecretsService:
    """
    Encrypted secret storage.

    Encryption: Fernet (AES-128-CBC + HMAC-SHA256).
    The passphrase is read from env SECRETS_PASSPHRASE.

    Fallback: if a secret is not found in the DB, checks env var
    SECRET_<SERVICE>_<NAME> (uppercased).
    """

    @staticmethod
    def _env_fallback(service: str, name: str) -> Optional[str]:
        key = f"SECRET_{service.upper().replace('-','_')}_{name.upper().replace('-','_')}"
        return os.getenv(key)

    @classmethod
    async def get(
        cls,
        db: AsyncSession,
        service: str,
        name: str,
    ) -> Optional[str]:
        """Return the decrypted secret value or None if not found."""
        # Env fallback first (allows injection without DB for unit tests/CI)
        env_val = cls._env_fallback(service, name)
        if env_val is not None:
            return env_val

        row = await db.execute(
            text("SELECT encrypted_value FROM secrets WHERE service=:s AND name=:n"),
            {"s": service, "n": name},
        )
        rec = row.fetchone()
        if not rec:
            return None

        try:
            f = _get_fernet()
            return f.decrypt(bytes(rec[0])).decode()
        except (InvalidToken, Exception) as e:
            logger.error("secret decryption failed", service=service, name=name, error=str(e))
            return None

    @classmethod
    async def set(
        cls,
        db: AsyncSession,
        service: str,
        name: str,
        value: str,
        description: Optional[str] = None,
        created_by: str = "system",
        expires_at: Optional[datetime] = None,
    ) -> None:
        """Encrypt and upsert a secret."""
        f = _get_fernet()
        ciphertext = f.encrypt(value.encode())

        await db.execute(text("""
            INSERT INTO secrets (service, name, encrypted_value, description, created_by, expires_at)
            VALUES (:s, :n, :ev, :d, :by, :exp)
            ON CONFLICT (service, name) DO UPDATE
              SET encrypted_value = EXCLUDED.encrypted_value,
                  version         = secrets.version + 1,
                  rotated_at      = NOW(),
                  updated_at      = NOW()
        """), {
            "s": service, "n": name, "ev": ciphertext,
            "d": description, "by": created_by, "exp": expires_at,
        })

    @classmethod
    async def delete(cls, db: AsyncSession, service: str, name: str) -> bool:
        result = await db.execute(
            text("DELETE FROM secrets WHERE service=:s AND name=:n RETURNING id"),
            {"s": service, "n": name},
        )
        return result.rowcount > 0

    @classmethod
    async def list_names(cls, db: AsyncSession, service: str) -> list[str]:
        """List secret names (NOT values) for a service."""
        rows = await db.execute(
            text("SELECT name FROM secrets WHERE service=:s ORDER BY name"), {"s": service}
        )
        return [r[0] for r in rows.fetchall()]


# ── Module-level singletons ───────────────────────────────────────────────────
config_service = ConfigService()
secrets_service = SecretsService()
