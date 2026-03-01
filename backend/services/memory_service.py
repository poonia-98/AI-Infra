"""
Agent Memory Service — short-term, long-term, episodic, and semantic memory
with pgvector-backed similarity search.
"""
from __future__ import annotations

import uuid
import json
from typing import Optional, List, Any
from datetime import datetime, timezone, timedelta

import structlog
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from fastapi import HTTPException

logger = structlog.get_logger()

MEMORY_TYPES = {"short_term", "long_term", "episodic", "semantic", "procedural"}


class MemoryService:

    @staticmethod
    async def store(
        db: AsyncSession,
        agent_id: str,
        content: str,
        memory_type: str = "short_term",
        key: Optional[str] = None,
        metadata: Optional[dict] = None,
        importance: float = 0.5,
        embedding: Optional[List[float]] = None,
        session_id: Optional[str] = None,
        organisation_id: Optional[str] = None,
        ttl_seconds: Optional[int] = None,
    ) -> str:
        if memory_type not in MEMORY_TYPES:
            raise HTTPException(400, f"Invalid memory_type. Must be one of: {MEMORY_TYPES}")

        expires_at = None
        if ttl_seconds:
            expires_at = datetime.now(timezone.utc) + timedelta(seconds=ttl_seconds)
        elif memory_type == "short_term":
            expires_at = datetime.now(timezone.utc) + timedelta(hours=24)

        meta_json = json.dumps(metadata or {})
        embedding_sql = None
        if embedding:
            # pgvector format: '[0.1, 0.2, ...]'
            embedding_sql = "[" + ",".join(str(v) for v in embedding) + "]"

        result = await db.execute(text("""
            INSERT INTO agent_memory
              (agent_id, organisation_id, memory_type, key, content,
               embedding, metadata, importance, session_id, expires_at)
            VALUES
              (:agent_id::uuid, :org_id::uuid, :memory_type, :key, :content,
               :embedding::vector, :metadata::jsonb, :importance, :session_id, :expires_at)
            RETURNING id::text
        """), {
            "agent_id": agent_id,
            "org_id": organisation_id,
            "memory_type": memory_type,
            "key": key,
            "content": content,
            "embedding": embedding_sql,
            "metadata": meta_json,
            "importance": importance,
            "session_id": session_id,
            "expires_at": expires_at,
        })
        row = result.fetchone()
        return row[0] if row else ""

    @staticmethod
    async def recall(
        db: AsyncSession,
        agent_id: str,
        memory_type: Optional[str] = None,
        key: Optional[str] = None,
        session_id: Optional[str] = None,
        limit: int = 50,
        include_expired: bool = False,
    ) -> List[dict]:
        conditions = ["agent_id=:agent_id::uuid"]
        params: dict = {"agent_id": agent_id, "limit": limit}

        if memory_type:
            conditions.append("memory_type=:memory_type")
            params["memory_type"] = memory_type
        if key:
            conditions.append("key=:key")
            params["key"] = key
        if session_id:
            conditions.append("session_id=:session_id")
            params["session_id"] = session_id
        if not include_expired:
            conditions.append("(expires_at IS NULL OR expires_at > NOW())")

        where = " AND ".join(conditions)
        rows = await db.execute(text(f"""
            UPDATE agent_memory
            SET access_count = access_count + 1, last_accessed = NOW()
            WHERE id IN (
                SELECT id FROM agent_memory
                WHERE {where}
                ORDER BY importance DESC, last_accessed DESC
                LIMIT :limit
            )
            RETURNING id::text, memory_type, key, content, metadata,
                      importance, access_count, session_id, created_at, expires_at
        """), params)

        memories = []
        for row in rows.fetchall():
            meta = row[3] if isinstance(row[3], dict) else {}
            memories.append({
                "id": row[0],
                "memory_type": row[1],
                "key": row[2],
                "content": row[3],
                "metadata": row[4],
                "importance": row[5],
                "access_count": row[6],
                "session_id": row[7],
                "created_at": row[8],
                "expires_at": row[9],
            })
        return memories

    @staticmethod
    async def search_similar(
        db: AsyncSession,
        agent_id: str,
        embedding: List[float],
        memory_type: Optional[str] = None,
        limit: int = 10,
        threshold: float = 0.7,
    ) -> List[dict]:
        """Vector similarity search using pgvector cosine distance."""
        embedding_str = "[" + ",".join(str(v) for v in embedding) + "]"
        params: dict = {
            "agent_id": agent_id,
            "embedding": embedding_str,
            "limit": limit,
            "threshold": 1 - threshold,  # cosine distance = 1 - similarity
        }

        type_filter = ""
        if memory_type:
            type_filter = "AND memory_type=:memory_type"
            params["memory_type"] = memory_type

        rows = await db.execute(text(f"""
            SELECT id::text, memory_type, key, content, metadata,
                   importance, (1 - (embedding <=> :embedding::vector)) as similarity
            FROM agent_memory
            WHERE agent_id=:agent_id::uuid
              AND embedding IS NOT NULL
              AND (expires_at IS NULL OR expires_at > NOW())
              {type_filter}
              AND (1 - (embedding <=> :embedding::vector)) >= (1 - :threshold)
            ORDER BY embedding <=> :embedding::vector
            LIMIT :limit
        """), params)

        results = []
        for row in rows.fetchall():
            results.append({
                "id": row[0],
                "memory_type": row[1],
                "key": row[2],
                "content": row[3],
                "metadata": row[4],
                "importance": row[5],
                "similarity": float(row[6]),
            })
        return results

    @staticmethod
    async def forget(
        db: AsyncSession,
        agent_id: str,
        memory_id: Optional[str] = None,
        key: Optional[str] = None,
        memory_type: Optional[str] = None,
        session_id: Optional[str] = None,
    ) -> int:
        conditions = ["agent_id=:agent_id::uuid"]
        params: dict = {"agent_id": agent_id}

        if memory_id:
            conditions.append("id=:id::uuid")
            params["id"] = memory_id
        if key:
            conditions.append("key=:key")
            params["key"] = key
        if memory_type:
            conditions.append("memory_type=:memory_type")
            params["memory_type"] = memory_type
        if session_id:
            conditions.append("session_id=:session_id")
            params["session_id"] = session_id

        result = await db.execute(
            text("DELETE FROM agent_memory WHERE " + " AND ".join(conditions) + " RETURNING id"),
            params,
        )
        return result.rowcount

    @staticmethod
    async def expire_short_term(db: AsyncSession) -> int:
        """Cleanup expired memories (called by background task)."""
        result = await db.execute(
            text("DELETE FROM agent_memory WHERE expires_at IS NOT NULL AND expires_at < NOW() RETURNING id")
        )
        return result.rowcount

    @staticmethod
    async def consolidate(
        db: AsyncSession,
        agent_id: str,
        session_id: str,
    ) -> int:
        """Promote important short-term memories to long-term (decay-weighted)."""
        result = await db.execute(text("""
            UPDATE agent_memory
            SET memory_type='long_term', expires_at=NULL,
                importance=LEAST(importance * 1.2, 1.0)
            WHERE agent_id=:agent_id::uuid
              AND session_id=:session_id
              AND memory_type='short_term'
              AND importance >= 0.7
            RETURNING id
        """), {"agent_id": agent_id, "session_id": session_id})
        return result.rowcount


memory_service = MemoryService()