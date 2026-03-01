import json
from typing import Any, Optional
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from services.nats_service import nats_service


class CommunicationService:
    async def send_message(
        self,
        db: AsyncSession,
        organisation_id: str,
        from_agent_id: str,
        message_type: str,
        payload: dict[str, Any],
        to_agent_id: Optional[str] = None,
        to_group: Optional[str] = None,
        subject: Optional[str] = None,
        correlation_id: Optional[str] = None,
        reply_to: Optional[str] = None,
        priority: int = 5,
        expires_at: Optional[str] = None,
    ) -> str:
        result = await db.execute(
            text(
                """
                INSERT INTO agent_messages
                  (from_agent_id, to_agent_id, to_group, organisation_id,
                   message_type, subject, payload, correlation_id, reply_to, priority, expires_at)
                VALUES
                  (:from_agent_id::uuid, :to_agent_id::uuid, :to_group, :organisation_id::uuid,
                   :message_type, :subject, :payload::jsonb, :correlation_id::uuid, :reply_to::uuid, :priority, :expires_at::timestamptz)
                RETURNING id::text
                """
            ),
            {
                "from_agent_id": from_agent_id,
                "to_agent_id": to_agent_id,
                "to_group": to_group,
                "organisation_id": organisation_id,
                "message_type": message_type,
                "subject": subject,
                "payload": json.dumps(payload or {}),
                "correlation_id": correlation_id,
                "reply_to": reply_to,
                "priority": priority,
                "expires_at": expires_at,
            },
        )
        row = result.fetchone()
        message_id = row[0] if row else ""

        event = {
            "id": message_id,
            "from_agent_id": from_agent_id,
            "to_agent_id": to_agent_id,
            "to_group": to_group,
            "message_type": message_type,
            "subject": subject,
            "payload": payload or {},
            "correlation_id": correlation_id,
            "priority": priority,
        }
        if to_agent_id:
            await nats_service.publish(f"agent.msg.{to_agent_id}", event)
        elif to_group:
            await nats_service.publish(f"agent.group.{to_group}", event)
        else:
            await nats_service.publish("agent.broadcast", event)
        return message_id

    async def get_inbox(
        self,
        db: AsyncSession,
        agent_id: str,
        organisation_id: str,
        status: str = "pending",
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        rows = await db.execute(
            text(
                """
                SELECT id::text, from_agent_id::text, to_agent_id::text, to_group, organisation_id::text,
                       message_type, subject, payload, correlation_id::text, reply_to::text,
                       status, priority, delivered_at, read_at, expires_at, created_at
                FROM agent_messages
                WHERE to_agent_id=:agent_id::uuid
                  AND organisation_id=:organisation_id::uuid
                  AND status=:status
                  AND (expires_at IS NULL OR expires_at > NOW())
                ORDER BY priority ASC, created_at ASC
                LIMIT :limit
                """
            ),
            {
                "agent_id": agent_id,
                "organisation_id": organisation_id,
                "status": status,
                "limit": limit,
            },
        )
        return [
            {
                "id": r[0],
                "from_agent_id": r[1],
                "to_agent_id": r[2],
                "to_group": r[3],
                "organisation_id": r[4],
                "message_type": r[5],
                "subject": r[6],
                "payload": r[7],
                "correlation_id": r[8],
                "reply_to": r[9],
                "status": r[10],
                "priority": r[11],
                "delivered_at": r[12],
                "read_at": r[13],
                "expires_at": r[14],
                "created_at": r[15],
            }
            for r in rows.fetchall()
        ]

    async def mark_read(self, db: AsyncSession, message_id: str, organisation_id: str) -> bool:
        result = await db.execute(
            text(
                """
                UPDATE agent_messages
                SET status='read', read_at=NOW()
                WHERE id=:message_id::uuid
                  AND organisation_id=:organisation_id::uuid
                """
            ),
            {"message_id": message_id, "organisation_id": organisation_id},
        )
        return result.rowcount > 0

    async def register_spawn(
        self,
        db: AsyncSession,
        organisation_id: str,
        parent_agent_id: str,
        child_agent_id: str,
        spawn_reason: Optional[str] = None,
        spawn_config: Optional[dict[str, Any]] = None,
    ) -> str:
        result = await db.execute(
            text(
                """
                INSERT INTO agent_spawn_tree
                  (parent_agent_id, child_agent_id, organisation_id, spawn_reason, spawn_config, status)
                VALUES
                  (:parent_agent_id::uuid, :child_agent_id::uuid, :organisation_id::uuid, :spawn_reason, :spawn_config::jsonb, 'active')
                ON CONFLICT (parent_agent_id, child_agent_id) DO UPDATE
                SET spawn_reason=EXCLUDED.spawn_reason,
                    spawn_config=EXCLUDED.spawn_config,
                    status='active'
                RETURNING id::text
                """
            ),
            {
                "parent_agent_id": parent_agent_id,
                "child_agent_id": child_agent_id,
                "organisation_id": organisation_id,
                "spawn_reason": spawn_reason,
                "spawn_config": json.dumps(spawn_config or {}),
            },
        )
        row = result.fetchone()
        return row[0] if row else ""

    async def get_children(self, db: AsyncSession, parent_agent_id: str, organisation_id: str) -> list[dict[str, Any]]:
        rows = await db.execute(
            text(
                """
                SELECT st.id::text, st.parent_agent_id::text, st.child_agent_id::text,
                       st.organisation_id::text, st.spawn_reason, st.spawn_config, st.status, st.created_at,
                       a.name, a.status
                FROM agent_spawn_tree st
                JOIN agents a ON a.id=st.child_agent_id
                WHERE st.parent_agent_id=:parent_agent_id::uuid
                  AND st.organisation_id=:organisation_id::uuid
                ORDER BY st.created_at DESC
                """
            ),
            {"parent_agent_id": parent_agent_id, "organisation_id": organisation_id},
        )
        return [
            {
                "id": r[0],
                "parent_agent_id": r[1],
                "child_agent_id": r[2],
                "organisation_id": r[3],
                "spawn_reason": r[4],
                "spawn_config": r[5],
                "status": r[6],
                "created_at": r[7],
                "child_name": r[8],
                "child_status": r[9],
            }
            for r in rows.fetchall()
        ]

    async def discover_agents(
        self,
        db: AsyncSession,
        organisation_id: str,
        group: Optional[str] = None,
        status: str = "running",
        limit: int = 200,
    ) -> list[dict[str, Any]]:
        clauses = ["status=:status", "organisation_id=:organisation_id::uuid"]
        params: dict[str, Any] = {
            "status": status,
            "organisation_id": organisation_id,
            "limit": limit,
        }
        if group:
            clauses.append("labels->>'group'=:group")
            params["group"] = group
        where_sql = " AND ".join(clauses)
        rows = await db.execute(
            text(
                f"""
                SELECT id::text, name, agent_type, image, status, labels, node_id, created_at
                FROM agents
                WHERE {where_sql}
                ORDER BY updated_at DESC
                LIMIT :limit
                """
            ),
            params,
        )
        return [
            {
                "id": r[0],
                "name": r[1],
                "agent_type": r[2],
                "image": r[3],
                "status": r[4],
                "labels": r[5],
                "node_id": r[6],
                "created_at": r[7],
            }
            for r in rows.fetchall()
        ]


communication_service = CommunicationService()
