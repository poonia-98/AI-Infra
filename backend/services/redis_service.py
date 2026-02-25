import json
from typing import Any, Optional
import redis.asyncio as redis
from config import settings


class RedisService:
    def __init__(self):
        self.client: Optional[redis.Redis] = None

    async def connect(self):
        self.client = redis.from_url(settings.REDIS_URL, decode_responses=True)
        await self.client.ping()

    async def disconnect(self):
        if self.client:
            await self.client.aclose()

    async def set_agent_state(self, agent_id: str, state: dict[str, Any], ttl: int = 3600):
        key = f"agent:state:{agent_id}"
        await self.client.setex(key, ttl, json.dumps(state))

    async def get_agent_state(self, agent_id: str) -> Optional[dict[str, Any]]:
        key = f"agent:state:{agent_id}"
        data = await self.client.get(key)
        return json.loads(data) if data else None

    async def delete_agent_state(self, agent_id: str):
        await self.client.delete(f"agent:state:{agent_id}")

    async def set_execution_state(self, execution_id: str, state: dict[str, Any], ttl: int = 7200):
        key = f"execution:state:{execution_id}"
        await self.client.setex(key, ttl, json.dumps(state))

    async def get_execution_state(self, execution_id: str) -> Optional[dict[str, Any]]:
        key = f"execution:state:{execution_id}"
        data = await self.client.get(key)
        return json.loads(data) if data else None

    async def cache_set(self, key: str, value: Any, ttl: int = 300):
        await self.client.setex(f"cache:{key}", ttl, json.dumps(value, default=str))

    async def cache_get(self, key: str) -> Optional[Any]:
        data = await self.client.get(f"cache:{key}")
        return json.loads(data) if data else None

    async def cache_delete(self, key: str):
        await self.client.delete(f"cache:{key}")

    async def publish(self, channel: str, message: dict[str, Any]):
        await self.client.publish(channel, json.dumps(message, default=str))

    async def increment_counter(self, key: str, ttl: int = 86400) -> int:
        pipe = self.client.pipeline()
        await pipe.incr(f"counter:{key}")
        await pipe.expire(f"counter:{key}", ttl)
        results = await pipe.execute()
        return results[0]

    async def get_all_agent_states(self) -> dict[str, Any]:
        keys = await self.client.keys("agent:state:*")
        states = {}
        for key in keys:
            data = await self.client.get(key)
            if data:
                agent_id = key.split(":")[-1]
                states[agent_id] = json.loads(data)
        return states

    async def set_streaming_state(self, stream_key: str, data: dict[str, Any]):
        key = f"stream:{stream_key}"
        await self.client.setex(key, 300, json.dumps(data, default=str))

    async def lpush_log(self, agent_id: str, log_entry: dict[str, Any], max_entries: int = 1000):
        key = f"logs:live:{agent_id}"
        await self.client.lpush(key, json.dumps(log_entry, default=str))
        await self.client.ltrim(key, 0, max_entries - 1)
        await self.client.expire(key, 3600)

    async def get_live_logs(self, agent_id: str, count: int = 100) -> list[dict[str, Any]]:
        key = f"logs:live:{agent_id}"
        entries = await self.client.lrange(key, 0, count - 1)
        return [json.loads(e) for e in entries]

    async def ping(self):
        if not self.client:
            return False
        try:
            await self.client.ping()
            return True
        except:
            return False
redis_service = RedisService()