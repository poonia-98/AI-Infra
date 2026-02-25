import json
import asyncio
from typing import Any, Callable, Optional
import nats
from nats.aio.client import Client as NATS
from config import settings
import structlog

logger = structlog.get_logger()


class NATSService:
    def __init__(self):
        self.nc: Optional[NATS] = None
        self.subscriptions: list = []

    async def connect(self):
        self.nc = await nats.connect(
            settings.NATS_URL,
            error_cb=self._error_handler,
            disconnected_cb=self._disconnected_handler,
            reconnected_cb=self._reconnected_handler,
        )
        logger.info("Connected to NATS", url=settings.NATS_URL)

    async def disconnect(self):
        for sub in self.subscriptions:
            try:
                await sub.unsubscribe()
            except Exception:
                pass
        if self.nc:
            await self.nc.drain()
            await self.nc.close()

    async def publish(self, subject: str, data: dict[str, Any]):
        if self.nc and not self.nc.is_closed:
            payload = json.dumps(data, default=str).encode()
            await self.nc.publish(subject, payload)

    async def subscribe(self, subject: str, handler: Callable, queue: str = ""):
        if self.nc and not self.nc.is_closed:
            sub = await self.nc.subscribe(subject, queue=queue, cb=handler)
            self.subscriptions.append(sub)
            return sub

    async def _error_handler(self, e):
        logger.error("NATS error", error=str(e))

    async def _disconnected_handler(self):
        logger.warning("NATS disconnected")

    async def _reconnected_handler(self):
        logger.info("NATS reconnected")
    def is_connected(self):
        return self.nc is not None and not self.nc.is_closed

nats_service = NATSService() 
            
