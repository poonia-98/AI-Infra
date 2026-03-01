"""
Time Travel Debugger — Backend Router
Provides full execution replay with step-level tracing.
Developers can view execution timelines, inspect every step,
rewind, edit prompts, and re-run from any point.
"""
import json
from typing import Any, Optional, List
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession
from database import get_db

router = APIRouter(prefix="/api/v1/time-travel", tags=["time-travel-debugger"])


# ── Pydantic Models ────────────────────────────────────────────────────────────

class ExecutionStepCreate(BaseModel):
    execution_id: str
    agent_id: str
    organisation_id: Optional[str] = None
    step_number: int
    step_type: str  # llm_call | tool_call | decision | memory_read | memory_write | http_call | output
    step_name: Optional[str] = None
    input_data: dict[str, Any] = Field(default_factory=dict)
    output_data: dict[str, Any] = Field(default_factory=dict)
    prompt: Optional[str] = None
    completion: Optional[str] = None
    model: Optional[str] = None
    prompt_tokens: Optional[int] = None
    completion_tokens: Optional[int] = None
    tool_name: Optional[str] = None
    tool_args: dict[str, Any] = Field(default_factory=dict)
    tool_result: dict[str, Any] = Field(default_factory=dict)
    decision_type: Optional[str] = None
    decision_reason: Optional[str] = None
    alternatives: List[Any] = Field(default_factory=list)
    duration_ms: Optional[int] = None
    cost_usd: float = 0
    status: str = "completed"
    error_message: Optional[str] = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class ReplaySessionCreate(BaseModel):
    original_exec_id: str
    agent_id: str
    organisation_id: Optional[str] = None
    replay_from_step: int = 0
    edited_steps: dict[str, Any] = Field(default_factory=dict)
    created_by: Optional[str] = None


class TimelineUpsert(BaseModel):
    execution_id: str
    agent_id: str
    organisation_id: Optional[str] = None
    total_steps: int = 0
    total_llm_calls: int = 0
    total_tool_calls: int = 0
    total_tokens: int = 0
    total_cost_usd: float = 0
    decision_graph: dict[str, Any] = Field(default_factory=dict)
    tool_usage: dict[str, Any] = Field(default_factory=dict)
    prompt_chain: List[Any] = Field(default_factory=list)


# ── Routes ─────────────────────────────────────────────────────────────────────

@router.post("/steps", status_code=201)
async def record_step(payload: ExecutionStepCreate, db: AsyncSession = Depends(get_db)):
    """Record a single execution step for time-travel replay."""
    result = await db.execute(
        text("""
            INSERT INTO execution_steps (
                execution_id, agent_id, organisation_id, step_number, step_type, step_name,
                input_data, output_data, prompt, completion, model,
                prompt_tokens, completion_tokens, tool_name, tool_args, tool_result,
                decision_type, decision_reason, alternatives,
                duration_ms, cost_usd, status, error_message, metadata
            ) VALUES (
                :execution_id::uuid, :agent_id::uuid, :org_id::uuid, :step_number, :step_type, :step_name,
                :input_data::jsonb, :output_data::jsonb, :prompt, :completion, :model,
                :prompt_tokens, :completion_tokens, :tool_name, :tool_args::jsonb, :tool_result::jsonb,
                :decision_type, :decision_reason, :alternatives::jsonb,
                :duration_ms, :cost_usd, :status, :error_message, :metadata::jsonb
            ) RETURNING id::text
        """),
        {
            "execution_id": payload.execution_id,
            "agent_id": payload.agent_id,
            "org_id": payload.organisation_id,
            "step_number": payload.step_number,
            "step_type": payload.step_type,
            "step_name": payload.step_name,
            "input_data": json.dumps(payload.input_data),
            "output_data": json.dumps(payload.output_data),
            "prompt": payload.prompt,
            "completion": payload.completion,
            "model": payload.model,
            "prompt_tokens": payload.prompt_tokens,
            "completion_tokens": payload.completion_tokens,
            "tool_name": payload.tool_name,
            "tool_args": json.dumps(payload.tool_args),
            "tool_result": json.dumps(payload.tool_result),
            "decision_type": payload.decision_type,
            "decision_reason": payload.decision_reason,
            "alternatives": json.dumps(payload.alternatives),
            "duration_ms": payload.duration_ms,
            "cost_usd": payload.cost_usd,
            "status": payload.status,
            "error_message": payload.error_message,
            "metadata": json.dumps(payload.metadata),
        },
    )
    row = result.fetchone()
    await db.commit()
    return {"id": row[0] if row else ""}


@router.get("/executions/{execution_id}/steps")
async def get_execution_steps(
    execution_id: str,
    step_type: Optional[str] = None,
    limit: int = Query(500, ge=1, le=1000),
    db: AsyncSession = Depends(get_db),
):
    """Get all steps for an execution in order — the full execution timeline."""
    clauses = ["execution_id = :exec_id::uuid"]
    params: dict[str, Any] = {"exec_id": execution_id, "limit": limit}
    if step_type:
        clauses.append("step_type = :step_type")
        params["step_type"] = step_type
    where = " AND ".join(clauses)
    rows = await db.execute(
        text(f"""
            SELECT
                id::text, execution_id::text, agent_id::text,
                step_number, step_type, step_name,
                input_data, output_data,
                prompt, completion, model, prompt_tokens, completion_tokens,
                tool_name, tool_args, tool_result,
                decision_type, decision_reason, alternatives,
                duration_ms, cost_usd, status, error_message,
                is_replayed, metadata, started_at, completed_at
            FROM execution_steps
            WHERE {where}
            ORDER BY step_number ASC
            LIMIT :limit
        """),
        params,
    )
    return [dict(r._mapping) for r in rows.fetchall()]


@router.get("/executions/{execution_id}/timeline")
async def get_timeline(execution_id: str, db: AsyncSession = Depends(get_db)):
    """Get the high-level execution timeline summary."""
    row = await db.execute(
        text("""
            SELECT
                id::text, execution_id::text, agent_id::text,
                total_steps, total_llm_calls, total_tool_calls,
                total_tokens, total_cost_usd,
                decision_graph, tool_usage, prompt_chain,
                created_at, updated_at
            FROM execution_timelines
            WHERE execution_id = :exec_id::uuid
        """),
        {"exec_id": execution_id},
    )
    result = row.fetchone()
    if not result:
        # Build it on-the-fly from steps
        steps_row = await db.execute(
            text("""
                SELECT
                    COUNT(*) as total_steps,
                    SUM(CASE WHEN step_type='llm_call' THEN 1 ELSE 0 END) as llm_calls,
                    SUM(CASE WHEN step_type='tool_call' THEN 1 ELSE 0 END) as tool_calls,
                    COALESCE(SUM(prompt_tokens + COALESCE(completion_tokens,0)), 0) as total_tokens,
                    COALESCE(SUM(cost_usd), 0) as total_cost
                FROM execution_steps WHERE execution_id = :exec_id::uuid
            """),
            {"exec_id": execution_id},
        )
        agg = steps_row.fetchone()
        if agg:
            return {
                "execution_id": execution_id,
                "total_steps": agg[0] or 0,
                "total_llm_calls": agg[1] or 0,
                "total_tool_calls": agg[2] or 0,
                "total_tokens": agg[3] or 0,
                "total_cost_usd": float(agg[4] or 0),
            }
        raise HTTPException(status_code=404, detail="Timeline not found")
    return dict(result._mapping)


@router.post("/timelines", status_code=201)
async def upsert_timeline(payload: TimelineUpsert, db: AsyncSession = Depends(get_db)):
    """Upsert an execution timeline record."""
    result = await db.execute(
        text("""
            INSERT INTO execution_timelines (
                execution_id, agent_id, organisation_id,
                total_steps, total_llm_calls, total_tool_calls,
                total_tokens, total_cost_usd,
                decision_graph, tool_usage, prompt_chain
            ) VALUES (
                :exec_id::uuid, :agent_id::uuid, :org_id::uuid,
                :total_steps, :total_llm_calls, :total_tool_calls,
                :total_tokens, :total_cost_usd,
                :decision_graph::jsonb, :tool_usage::jsonb, :prompt_chain::jsonb
            )
            ON CONFLICT (execution_id) DO UPDATE SET
                total_steps = EXCLUDED.total_steps,
                total_llm_calls = EXCLUDED.total_llm_calls,
                total_tool_calls = EXCLUDED.total_tool_calls,
                total_tokens = EXCLUDED.total_tokens,
                total_cost_usd = EXCLUDED.total_cost_usd,
                decision_graph = EXCLUDED.decision_graph,
                tool_usage = EXCLUDED.tool_usage,
                prompt_chain = EXCLUDED.prompt_chain,
                updated_at = NOW()
            RETURNING id::text
        """),
        {
            "exec_id": payload.execution_id,
            "agent_id": payload.agent_id,
            "org_id": payload.organisation_id,
            "total_steps": payload.total_steps,
            "total_llm_calls": payload.total_llm_calls,
            "total_tool_calls": payload.total_tool_calls,
            "total_tokens": payload.total_tokens,
            "total_cost_usd": payload.total_cost_usd,
            "decision_graph": json.dumps(payload.decision_graph),
            "tool_usage": json.dumps(payload.tool_usage),
            "prompt_chain": json.dumps(payload.prompt_chain),
        },
    )
    row = result.fetchone()
    await db.commit()
    return {"id": row[0] if row else ""}


@router.post("/replay", status_code=201)
async def create_replay_session(
    payload: ReplaySessionCreate,
    db: AsyncSession = Depends(get_db),
):
    """Create a replay session to re-run execution from a specific step."""
    result = await db.execute(
        text("""
            INSERT INTO replay_sessions (
                original_exec_id, agent_id, organisation_id,
                replay_from_step, edited_steps, created_by, status
            ) VALUES (
                :orig_id::uuid, :agent_id::uuid, :org_id::uuid,
                :replay_from, :edited::jsonb, :created_by, 'pending'
            ) RETURNING id::text
        """),
        {
            "orig_id": payload.original_exec_id,
            "agent_id": payload.agent_id,
            "org_id": payload.organisation_id,
            "replay_from": payload.replay_from_step,
            "edited": json.dumps(payload.edited_steps),
            "created_by": payload.created_by,
        },
    )
    row = result.fetchone()
    await db.commit()
    return {"id": row[0] if row else "", "status": "pending"}


@router.get("/replay/{session_id}")
async def get_replay_session(session_id: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(
        text("""
            SELECT id::text, original_exec_id::text, new_exec_id::text,
                   agent_id::text, replay_from_step, edited_steps, status,
                   created_by, created_at, completed_at
            FROM replay_sessions WHERE id = :id::uuid
        """),
        {"id": session_id},
    )
    result = row.fetchone()
    if not result:
        raise HTTPException(status_code=404, detail="Replay session not found")
    return dict(result._mapping)


@router.get("/agents/{agent_id}/executions")
async def list_agent_executions_with_steps(
    agent_id: str,
    limit: int = Query(20, ge=1, le=100),
    db: AsyncSession = Depends(get_db),
):
    """List executions for an agent with step count — for the time travel UI."""
    rows = await db.execute(
        text("""
            SELECT
                e.id::text,
                e.status,
                e.started_at,
                e.completed_at,
                e.duration_ms,
                e.error,
                COALESCE(et.total_steps, 0) as total_steps,
                COALESCE(et.total_llm_calls, 0) as total_llm_calls,
                COALESCE(et.total_tool_calls, 0) as total_tool_calls,
                COALESCE(et.total_tokens, 0) as total_tokens,
                COALESCE(et.total_cost_usd, 0) as total_cost_usd
            FROM executions e
            LEFT JOIN execution_timelines et ON et.execution_id = e.id
            WHERE e.agent_id = :agent_id::uuid
            ORDER BY e.created_at DESC
            LIMIT :limit
        """),
        {"agent_id": agent_id, "limit": limit},
    )
    return [dict(r._mapping) for r in rows.fetchall()]


@router.get("/step/{step_id}")
async def get_step(step_id: str, db: AsyncSession = Depends(get_db)):
    """Get a single execution step by ID."""
    row = await db.execute(
        text("""
            SELECT * FROM execution_steps WHERE id = :id::uuid
        """),
        {"id": step_id},
    )
    result = row.fetchone()
    if not result:
        raise HTTPException(status_code=404, detail="Step not found")
    return dict(result._mapping)


@router.patch("/step/{step_id}/edit")
async def edit_step(
    step_id: str,
    updates: dict[str, Any],
    db: AsyncSession = Depends(get_db),
):
    """Edit a step's input/prompt for replay (marks as replay candidate)."""
    allowed = {"prompt", "input_data", "tool_args", "decision_type"}
    valid_updates = {k: v for k, v in updates.items() if k in allowed}
    if not valid_updates:
        raise HTTPException(status_code=400, detail="No valid fields to update")

    set_clauses = []
    params: dict[str, Any] = {"id": step_id}
    for k, v in valid_updates.items():
        if k in ("input_data", "tool_args"):
            set_clauses.append(f"{k} = :{k}::jsonb")
            params[k] = json.dumps(v)
        else:
            set_clauses.append(f"{k} = :{k}")
            params[k] = v

    await db.execute(
        text(f"UPDATE execution_steps SET {', '.join(set_clauses)} WHERE id = :id::uuid"),
        params,
    )
    await db.commit()
    return {"status": "updated"}