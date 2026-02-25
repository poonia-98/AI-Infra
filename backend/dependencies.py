from typing import AsyncGenerator
from sqlalchemy.ext.asyncio import AsyncSession
from database import AsyncSessionLocal
from services.redis_service import redis_service, RedisService
from services.nats_service import nats_service, NATSService


async def get_db() -> AsyncGenerator[AsyncSession, None]:
    async with AsyncSessionLocal() as session:
        try:
            yield session
            await session.commit()
        except Exception:
            await session.rollback()
            raise
        finally:
            await session.close()


async def get_redis() -> RedisService:
    return redis_service


async def get_nats() -> NATSService:
    return nats_service