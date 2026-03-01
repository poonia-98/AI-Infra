import hashlib
from datetime import datetime, timedelta, timezone
from typing import Any
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from services.redis_service import redis_service


class RateLimiter:
    async def allow(
        self,
        db: AsyncSession,
        key: str,
        limit: int,
        window_seconds: int,
    ) -> dict[str, Any]:
        if limit <= 0:
            return {"allowed": False, "remaining": 0, "reset_in_seconds": window_seconds}
        if window_seconds <= 0:
            window_seconds = 60

        redis_key = f"ratelimit:{key}:{window_seconds}"
        if redis_service.client:
            current = await redis_service.client.incr(redis_key)
            if current == 1:
                await redis_service.client.expire(redis_key, window_seconds)
            allowed = current <= limit
            remaining = max(limit - current, 0)
            ttl = await redis_service.client.ttl(redis_key)
            return {
                "allowed": allowed,
                "remaining": remaining,
                "limit": limit,
                "current": current,
                "reset_in_seconds": ttl if ttl > 0 else window_seconds,
                "source": "redis",
            }

        now = datetime.now(timezone.utc)
        stable_key = hashlib.sha256(f"{key}:{window_seconds}".encode()).hexdigest()
        row = await db.execute(
            text(
                """
                INSERT INTO rate_limit_counters (key, count, window_start, window_end, updated_at)
                VALUES (:key, 1, NOW(), NOW() + (:window_seconds || ' seconds')::interval, NOW())
                ON CONFLICT (key) DO UPDATE
                SET
                  count = CASE
                    WHEN rate_limit_counters.window_end <= NOW() THEN 1
                    ELSE rate_limit_counters.count + 1
                  END,
                  window_start = CASE
                    WHEN rate_limit_counters.window_end <= NOW() THEN NOW()
                    ELSE rate_limit_counters.window_start
                  END,
                  window_end = CASE
                    WHEN rate_limit_counters.window_end <= NOW()
                      THEN NOW() + (:window_seconds || ' seconds')::interval
                    ELSE rate_limit_counters.window_end
                  END,
                  updated_at = NOW()
                RETURNING count, window_end
                """
            ),
            {"key": stable_key, "window_seconds": window_seconds},
        )
        rec = row.fetchone()
        count = int(rec[0]) if rec else 1
        current_end = rec[1] if rec else (now + timedelta(seconds=window_seconds))
        allowed = count <= limit
        remaining = max(limit - count, 0)
        reset_seconds = max(int((current_end - now).total_seconds()), 1)
        return {
            "allowed": allowed,
            "remaining": remaining,
            "limit": limit,
            "current": count,
            "reset_in_seconds": reset_seconds,
            "source": "db",
        }


rate_limiter = RateLimiter()
