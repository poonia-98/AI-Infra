"""
Audit logging middleware.

Records every mutating API call (POST, PUT, PATCH, DELETE) to the audit_logs table.
The middleware runs AFTER the response is sent so it never blocks the request.
"""
import time
import uuid
import json
from datetime import datetime, timezone
from typing import Optional
from starlette.middleware.base import BaseHTTPMiddleware
from starlette.requests import Request
from starlette.responses import Response
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
import structlog

from database import AsyncSessionLocal
from services.task_registry import task_registry

logger = structlog.get_logger()

# Actions to audit by method + path pattern
_AUDITED_METHODS = {"POST", "PUT", "PATCH", "DELETE"}

# Skip these paths (health / metrics / read-only)
_SKIP_PREFIXES = ("/health", "/ready", "/live", "/metrics", "/docs", "/openapi", "/api/v1/logs", "/api/v1/events")


def _extract_action(method: str, path: str) -> str:
    """Derive a human-readable audit action from the HTTP method + path."""
    parts = [p for p in path.split("/") if p and p not in ("api", "v1", "v2")]
    resource = parts[0] if parts else "unknown"
    sub = parts[2] if len(parts) >= 3 else None

    mapping = {
        ("POST",   None):    f"{resource}.created",
        ("PUT",    None):    f"{resource}.updated",
        ("PATCH",  None):    f"{resource}.updated",
        ("DELETE", None):    f"{resource}.deleted",
        ("POST",   "start"): f"{resource}.started",
        ("POST",   "stop"):  f"{resource}.stopped",
        ("POST",   "restart"): f"{resource}.restarted",
        ("POST",   "resolve"): f"{resource}.alert_resolved",
        ("POST",   "heartbeat"): "node.heartbeat",
        ("POST",   "drain"):   "node.drained",
    }
    return mapping.get((method, sub)) or mapping.get((method, None)) or f"{resource}.{method.lower()}"


def _extract_resource_id(path: str) -> Optional[str]:
    """Extract UUID resource ID from path like /api/v1/agents/{id}/..."""
    parts = path.split("/")
    for part in parts:
        try:
            uuid.UUID(part)
            return part
        except ValueError:
            continue
    return None


def _extract_resource(path: str) -> str:
    parts = [p for p in path.split("/") if p and p not in ("api", "v1", "v2")]
    return parts[0] if parts else "unknown"


class AuditMiddleware(BaseHTTPMiddleware):
    """
    FastAPI middleware that appends an audit_logs row for every mutating request.

    It reads the actor from authenticated request context.
    """

    async def dispatch(self, request: Request, call_next):
        # Only audit mutating methods
        if request.method not in _AUDITED_METHODS:
            return await call_next(request)

        path = request.url.path
        if any(path.startswith(prefix) for prefix in _SKIP_PREFIXES):
            return await call_next(request)

        start = time.monotonic()
        response: Response = await call_next(request)
        duration_ms = int((time.monotonic() - start) * 1000)

        status = "success" if response.status_code < 400 else "failure"
        user = getattr(request.state, "user", None)
        if user:
            actor_id = user.user_id
            actor_type = "user"
        else:
            actor_id = "system"
            actor_type = "service"
        actor_ip_str = request.headers.get("X-Forwarded-For") or (
            request.client.host if request.client else None
        )
        request_id = request.headers.get("X-Request-ID", str(uuid.uuid4()))
        action = _extract_action(request.method, path)
        resource = _extract_resource(path)
        resource_id = _extract_resource_id(path)

        # Fire-and-forget — never block the response
        task_registry.spawn(_write_audit_log(
            actor_id=actor_id,
            actor_type=actor_type,
            actor_ip=actor_ip_str,
            action=action,
            resource=resource,
            resource_id=resource_id,
            new_value={"path": path, "method": request.method,
                       "status_code": response.status_code, "duration_ms": duration_ms},
            metadata={"request_id": request_id},
            request_id=request_id,
            status=status,
        ))

        return response


async def _write_audit_log(
    actor_id: str,
    actor_type: str,
    actor_ip: Optional[str],
    action: str,
    resource: str,
    resource_id: Optional[str],
    new_value: dict,
    metadata: dict,
    request_id: str,
    status: str,
    error: Optional[str] = None,
    service: str = "backend",
):
    try:
        async with AsyncSessionLocal() as db:
            old_json = json.dumps({})
            new_json = json.dumps(new_value)
            meta_json = json.dumps(metadata)
            ip_val = actor_ip if actor_ip else None
            rid = None
            if resource_id:
                try:
                    rid = str(uuid.UUID(resource_id))
                except ValueError:
                    pass

            await db.execute(text("""
                INSERT INTO audit_logs
                  (actor_id, actor_type, actor_ip, action, resource, resource_id,
                   old_value, new_value, metadata, request_id, service, status, error, created_at)
                VALUES
                  (:actor_id, :actor_type, :actor_ip::inet, :action, :resource, :resource_id::uuid,
                   :old_value::jsonb, :new_value::jsonb, :metadata::jsonb,
                   :request_id, :service, :status, :error, NOW())
            """), {
                "actor_id": actor_id,
                "actor_type": actor_type,
                "actor_ip": ip_val,
                "action": action,
                "resource": resource,
                "resource_id": rid,
                "old_value": old_json,
                "new_value": new_json,
                "metadata": meta_json,
                "request_id": request_id,
                "service": service,
                "status": status,
                "error": error,
            })
            await db.commit()
    except Exception as e:
        logger.warning("audit_log write failed", error=str(e), action=action)


# ── Direct audit helper (call from service layer for precise old/new values) ──

async def audit(
    action: str,
    resource: str,
    resource_id: Optional[str] = None,
    old_value: Optional[dict] = None,
    new_value: Optional[dict] = None,
    actor_id: str = "system",
    actor_type: str = "service",
    service: str = "backend",
    metadata: Optional[dict] = None,
):
    """Write an audit log entry directly from the service layer."""
    await _write_audit_log(
        actor_id=actor_id,
        actor_type=actor_type,
        actor_ip=None,
        action=action,
        resource=resource,
        resource_id=resource_id,
        new_value=new_value or {},
        metadata=metadata or {},
        request_id=str(uuid.uuid4()),
        status="success",
        service=service,
    )
