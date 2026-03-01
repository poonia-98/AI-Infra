import asyncio
import hashlib
import hmac
import json
from datetime import datetime, timezone, timedelta
from typing import Any, Optional

import httpx
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from config import settings
from database import AsyncSessionLocal
from security.url_guard import validate_outbound_url
from services.task_registry import task_registry


class WebhookService:
    def _hash_secret(self, secret: str) -> str:
        return hashlib.sha256(secret.encode()).hexdigest()

    async def _allowed_domains(self, db: AsyncSession) -> list[str]:
        domains = [x.strip() for x in settings.WEBHOOK_ALLOWED_DOMAINS.split(",") if x.strip()]
        rows = await db.execute(
            text(
                """
                SELECT value
                FROM configs
                WHERE service='webhooks'
                  AND key='allowed_domains'
                LIMIT 1
                """
            )
        )
        row = rows.fetchone()
        if row and row[0]:
            domains.extend([x.strip() for x in str(row[0]).split(",") if x.strip()])
        # de-duplicate while preserving order
        seen: set[str] = set()
        out: list[str] = []
        for domain in domains:
            d = domain.lower()
            if d not in seen:
                seen.add(d)
                out.append(d)
        return out

    async def create_webhook(
        self,
        db: AsyncSession,
        organisation_id: str,
        name: str,
        endpoint: str,
        events: list[str],
        agent_id: Optional[str] = None,
        workflow_id: Optional[str] = None,
        secret: Optional[str] = None,
    ) -> str:
        secret_hash = self._hash_secret(secret) if secret else None
        pg_events = "{" + ",".join(events or []) + "}"
        result = await db.execute(
            text(
                """
                INSERT INTO webhooks
                  (organisation_id, agent_id, workflow_id, name, endpoint_path, secret_hash, events, enabled)
                VALUES
                  (:organisation_id::uuid, :agent_id::uuid, :workflow_id::uuid, :name, :endpoint, :secret_hash, :events::text[], TRUE)
                RETURNING id::text
                """
            ),
            {
                "organisation_id": organisation_id,
                "agent_id": agent_id,
                "workflow_id": workflow_id,
                "name": name,
                "endpoint": endpoint,
                "secret_hash": secret_hash,
                "events": pg_events,
            },
        )
        row = result.fetchone()
        return row[0] if row else ""

    async def list_webhooks(self, db: AsyncSession, organisation_id: Optional[str] = None) -> list[dict[str, Any]]:
        clauses = ["1=1"]
        params: dict[str, Any] = {}
        if organisation_id:
            clauses.append("organisation_id=:organisation_id::uuid")
            params["organisation_id"] = organisation_id
        where = " AND ".join(clauses)
        rows = await db.execute(
            text(
                f"""
                SELECT id::text, organisation_id::text, agent_id::text, workflow_id::text,
                       name, endpoint_path, events, enabled, trigger_count, last_triggered, created_at
                FROM webhooks
                WHERE {where}
                ORDER BY created_at DESC
                """
            ),
            params,
        )
        return [
            {
                "id": r[0],
                "organisation_id": r[1],
                "agent_id": r[2],
                "workflow_id": r[3],
                "name": r[4],
                "endpoint": r[5],
                "events": r[6],
                "enabled": r[7],
                "trigger_count": r[8],
                "last_triggered": r[9],
                "created_at": r[10],
            }
            for r in rows.fetchall()
        ]

    async def disable_webhook(self, db: AsyncSession, webhook_id: str) -> bool:
        result = await db.execute(
            text("UPDATE webhooks SET enabled=FALSE WHERE id=:id::uuid"),
            {"id": webhook_id},
        )
        return result.rowcount > 0

    async def list_deliveries(self, db: AsyncSession, webhook_id: Optional[str] = None, limit: int = 100) -> list[dict[str, Any]]:
        clauses = ["1=1"]
        params: dict[str, Any] = {"limit": limit}
        if webhook_id:
            clauses.append("webhook_id=:webhook_id::uuid")
            params["webhook_id"] = webhook_id
        where = " AND ".join(clauses)
        rows = await db.execute(
            text(
                f"""
                SELECT id::text, webhook_id::text, event_type, target_url,
                       response_status, attempt, max_attempts, delivered,
                       next_attempt_at, delivered_at, last_error, created_at
                FROM webhook_deliveries
                WHERE {where}
                ORDER BY created_at DESC
                LIMIT :limit
                """
            ),
            params,
        )
        return [
            {
                "id": r[0],
                "webhook_id": r[1],
                "event_type": r[2],
                "target_url": r[3],
                "response_status": r[4],
                "attempt": r[5],
                "max_attempts": r[6],
                "delivered": r[7],
                "next_attempt_at": r[8],
                "delivered_at": r[9],
                "last_error": r[10],
                "created_at": r[11],
            }
            for r in rows.fetchall()
        ]

    async def dispatch_event(
        self,
        db: AsyncSession,
        organisation_id: str,
        event_type: str,
        payload: dict[str, Any],
        event_id: Optional[str] = None,
    ) -> int:
        allowed_domains = await self._allowed_domains(db)
        rows = await db.execute(
            text(
                """
                SELECT id::text, endpoint_path, secret_hash
                FROM webhooks
                WHERE enabled=TRUE
                  AND organisation_id=:organisation_id::uuid
                  AND :event_type = ANY(events)
                """
            ),
            {"event_type": event_type, "organisation_id": organisation_id},
        )
        hooks = rows.fetchall()
        dispatched = 0
        for hook in hooks:
            validate_outbound_url(hook[1], allowed_domains)
            delivery = await db.execute(
                text(
                    """
                    INSERT INTO webhook_deliveries
                      (webhook_id, event_type, event_id, target_url, payload, request_headers,
                       attempt, max_attempts, delivered, next_attempt_at)
                    VALUES
                      (:webhook_id::uuid, :event_type, :event_id::uuid, :target_url,
                       :payload::jsonb, '{}'::jsonb, 1, 5, FALSE, NOW())
                    RETURNING id::text
                    """
                ),
                {
                    "webhook_id": hook[0],
                    "event_type": event_type,
                    "event_id": event_id,
                    "target_url": hook[1],
                    "payload": json.dumps(payload),
                },
            )
            row = delivery.fetchone()
            if not row:
                continue
            delivery_id = row[0]
            dispatched += 1
            task_registry.spawn(self._deliver_once(delivery_id, hook[1], event_type, payload, 1, 5, hook[2]))
        return dispatched

    async def retry_pending(self, db: AsyncSession, limit: int = 100) -> int:
        rows = await db.execute(
            text(
                """
                SELECT wd.id::text, wd.target_url, wd.event_type, wd.payload, wd.attempt, wd.max_attempts,
                       w.secret_hash
                FROM webhook_deliveries wd
                JOIN webhooks w ON w.id=wd.webhook_id
                WHERE wd.delivered=FALSE
                  AND wd.attempt < wd.max_attempts
                  AND (wd.next_attempt_at IS NULL OR wd.next_attempt_at <= NOW())
                ORDER BY wd.created_at ASC
                LIMIT :limit
                """
            ),
            {"limit": limit},
        )
        deliveries = rows.fetchall()
        for d in deliveries:
            task_registry.spawn(self._deliver_once(d[0], d[1], d[2], d[3], d[4] + 1, d[5], d[6]))
        return len(deliveries)

    async def _deliver_once(
        self,
        delivery_id: str,
        target_url: str,
        event_type: str,
        payload: Any,
        attempt: int,
        max_attempts: int,
        secret_hash: Optional[str],
    ) -> None:
        body = json.dumps(payload).encode()
        signature = hmac.new(settings.SECRET_KEY.encode(), body, hashlib.sha256).hexdigest()
        headers = {
            "Content-Type": "application/json",
            "X-AI-Event": event_type,
            "X-AI-Delivery-ID": delivery_id,
            "X-AI-Signature": f"sha256={signature}",
        }
        if secret_hash:
            headers["X-AI-Webhook-Secret-Hash"] = secret_hash

        status_code: Optional[int] = None
        response_body = ""
        delivered = False
        error_text = None
        try:
            async with httpx.AsyncClient(timeout=15.0, follow_redirects=False) as client:
                response = await client.post(target_url, content=body, headers=headers)
                status_code = response.status_code
                response_body = response.text[:2000]
                delivered = 200 <= response.status_code < 300
                if not delivered:
                    error_text = f"http_{response.status_code}"
        except Exception as exc:
            error_text = str(exc)

        async with AsyncSessionLocal() as db:
            if delivered:
                await db.execute(
                    text(
                        """
                        UPDATE webhook_deliveries
                        SET delivered=TRUE,
                            response_status=:status,
                            response_body=:response_body,
                            attempt=:attempt,
                            delivered_at=NOW(),
                            next_attempt_at=NULL,
                            last_error=NULL,
                            updated_at=NOW()
                        WHERE id=:id::uuid
                        """
                    ),
                    {
                        "id": delivery_id,
                        "status": status_code,
                        "response_body": response_body,
                        "attempt": attempt,
                    },
                )
                await db.execute(
                    text(
                        """
                        UPDATE webhooks
                        SET trigger_count=trigger_count+1,
                            last_triggered=NOW()
                        WHERE id=(SELECT webhook_id FROM webhook_deliveries WHERE id=:id::uuid)
                        """
                    ),
                    {"id": delivery_id},
                )
            else:
                next_attempt = datetime.now(timezone.utc) + timedelta(seconds=min(300, 2 ** attempt))
                await db.execute(
                    text(
                        """
                        UPDATE webhook_deliveries
                        SET delivered=FALSE,
                            response_status=:status,
                            response_body=:response_body,
                            attempt=:attempt,
                            next_attempt_at=:next_attempt,
                            last_error=:last_error,
                            updated_at=NOW()
                        WHERE id=:id::uuid
                        """
                    ),
                    {
                        "id": delivery_id,
                        "status": status_code,
                        "response_body": response_body,
                        "attempt": attempt,
                        "next_attempt": next_attempt if attempt < max_attempts else None,
                        "last_error": error_text,
                    },
                )
            await db.commit()


webhook_service = WebhookService()
