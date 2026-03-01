import json
import uuid
from typing import Any, Optional
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text


class WorkflowEngineService:
    async def create_workflow(
        self,
        db: AsyncSession,
        name: str,
        dag: dict[str, Any],
        organisation_id: Optional[str] = None,
        description: Optional[str] = None,
        trigger_type: str = "manual",
        trigger_config: Optional[dict[str, Any]] = None,
        created_by: Optional[str] = None,
    ) -> str:
        result = await db.execute(
            text(
                """
                INSERT INTO workflows
                  (organisation_id, name, description, dag, trigger_type, trigger_config, status, created_by)
                VALUES
                  (:organisation_id::uuid, :name, :description, :dag::jsonb, :trigger_type, :trigger_config::jsonb, 'active', :created_by)
                RETURNING id::text
                """
            ),
            {
                "organisation_id": organisation_id,
                "name": name,
                "description": description,
                "dag": json.dumps(dag or {"nodes": [], "edges": []}),
                "trigger_type": trigger_type,
                "trigger_config": json.dumps(trigger_config or {}),
                "created_by": created_by,
            },
        )
        row = result.fetchone()
        return row[0] if row else ""

    async def list_workflows(
        self,
        db: AsyncSession,
        organisation_id: Optional[str] = None,
        status: Optional[str] = None,
        limit: int = 100,
    ) -> list[dict[str, Any]]:
        clauses = ["1=1"]
        params: dict[str, Any] = {"limit": limit}
        if organisation_id:
            clauses.append("organisation_id=:organisation_id::uuid")
            params["organisation_id"] = organisation_id
        if status:
            clauses.append("status=:status")
            params["status"] = status
        where_sql = " AND ".join(clauses)
        rows = await db.execute(
            text(
                f"""
                SELECT id::text, organisation_id::text, name, description, dag,
                       trigger_type, trigger_config, status, version, max_retries,
                       timeout_seconds, created_by, created_at, updated_at
                FROM workflows
                WHERE {where_sql}
                ORDER BY updated_at DESC
                LIMIT :limit
                """
            ),
            params,
        )
        out: list[dict[str, Any]] = []
        for r in rows.fetchall():
            out.append(
                {
                    "id": r[0],
                    "organisation_id": r[1],
                    "name": r[2],
                    "description": r[3],
                    "dag": r[4],
                    "trigger_type": r[5],
                    "trigger_config": r[6],
                    "status": r[7],
                    "version": r[8],
                    "max_retries": r[9],
                    "timeout_seconds": r[10],
                    "created_by": r[11],
                    "created_at": r[12],
                    "updated_at": r[13],
                }
            )
        return out

    async def get_workflow(self, db: AsyncSession, workflow_id: str) -> Optional[dict[str, Any]]:
        row = await db.execute(
            text(
                """
                SELECT id::text, organisation_id::text, name, description, dag,
                       trigger_type, trigger_config, status, version, max_retries,
                       timeout_seconds, created_by, created_at, updated_at
                FROM workflows
                WHERE id=:workflow_id::uuid
                """
            ),
            {"workflow_id": workflow_id},
        )
        r = row.fetchone()
        if not r:
            return None
        return {
            "id": r[0],
            "organisation_id": r[1],
            "name": r[2],
            "description": r[3],
            "dag": r[4],
            "trigger_type": r[5],
            "trigger_config": r[6],
            "status": r[7],
            "version": r[8],
            "max_retries": r[9],
            "timeout_seconds": r[10],
            "created_by": r[11],
            "created_at": r[12],
            "updated_at": r[13],
        }

    async def update_workflow(self, db: AsyncSession, workflow_id: str, patch: dict[str, Any]) -> bool:
        allowed = {
            "name": "name",
            "description": "description",
            "dag": "dag",
            "trigger_type": "trigger_type",
            "trigger_config": "trigger_config",
            "status": "status",
            "max_retries": "max_retries",
            "timeout_seconds": "timeout_seconds",
        }
        set_parts: list[str] = []
        params: dict[str, Any] = {"workflow_id": workflow_id}
        for key, value in patch.items():
            column = allowed.get(key)
            if not column:
                continue
            param_name = f"v_{key}"
            if key in {"dag", "trigger_config"}:
                set_parts.append(f"{column}=:{param_name}::jsonb")
                params[param_name] = json.dumps(value if value is not None else {})
            else:
                set_parts.append(f"{column}=:{param_name}")
                params[param_name] = value
        if not set_parts:
            return False
        set_parts.append("version=version+1")
        set_parts.append("updated_at=NOW()")
        result = await db.execute(
            text(
                f"""
                UPDATE workflows
                SET {", ".join(set_parts)}
                WHERE id=:workflow_id::uuid
                """
            ),
            params,
        )
        return result.rowcount > 0

    async def trigger_execution(
        self,
        db: AsyncSession,
        workflow_id: str,
        input_payload: Optional[dict[str, Any]] = None,
        triggered_by: Optional[str] = None,
        organisation_id: Optional[str] = None,
    ) -> str:
        result = await db.execute(
            text(
                """
                INSERT INTO workflow_executions
                  (workflow_id, organisation_id, status, input, context, triggered_by)
                VALUES
                  (:workflow_id::uuid, :organisation_id::uuid, 'pending', :input::jsonb, :context::jsonb, :triggered_by)
                RETURNING id::text
                """
            ),
            {
                "workflow_id": workflow_id,
                "organisation_id": organisation_id,
                "input": json.dumps(input_payload or {}),
                "context": json.dumps(input_payload or {}),
                "triggered_by": triggered_by,
            },
        )
        row = result.fetchone()
        return row[0] if row else ""

    async def get_execution(self, db: AsyncSession, execution_id: str) -> Optional[dict[str, Any]]:
        rows = await db.execute(
            text(
                """
                SELECT id::text, workflow_id::text, organisation_id::text, status,
                       input, output, context, error, triggered_by,
                       started_at, completed_at, duration_ms, created_at, updated_at
                FROM workflow_executions
                WHERE id=:execution_id::uuid
                """
            ),
            {"execution_id": execution_id},
        )
        r = rows.fetchone()
        if not r:
            return None
        return {
            "id": r[0],
            "workflow_id": r[1],
            "organisation_id": r[2],
            "status": r[3],
            "input": r[4],
            "output": r[5],
            "context": r[6],
            "error": r[7],
            "triggered_by": r[8],
            "started_at": r[9],
            "completed_at": r[10],
            "duration_ms": r[11],
            "created_at": r[12],
            "updated_at": r[13],
        }

    async def list_node_executions(self, db: AsyncSession, execution_id: str) -> list[dict[str, Any]]:
        rows = await db.execute(
            text(
                """
                SELECT id::text, workflow_exec_id::text, node_id, node_type, agent_id::text,
                       execution_id::text, status, input, output, error, attempt,
                       started_at, completed_at, duration_ms, created_at
                FROM workflow_node_executions
                WHERE workflow_exec_id=:execution_id::uuid
                ORDER BY created_at ASC
                """
            ),
            {"execution_id": execution_id},
        )
        return [
            {
                "id": r[0],
                "workflow_exec_id": r[1],
                "node_id": r[2],
                "node_type": r[3],
                "agent_id": r[4],
                "execution_id": r[5],
                "status": r[6],
                "input": r[7],
                "output": r[8],
                "error": r[9],
                "attempt": r[10],
                "started_at": r[11],
                "completed_at": r[12],
                "duration_ms": r[13],
                "created_at": r[14],
            }
            for r in rows.fetchall()
        ]


workflow_engine_service = WorkflowEngineService()
