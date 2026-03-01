import json
from typing import Any, Optional

from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession

from database import get_db


router = APIRouter(prefix="/api/v1/governance", tags=["governance"])


class PolicyCreate(BaseModel):
    organisation_id: Optional[str] = None
    name: str
    scope: str = "global"
    policy_type: str = "guardrail"
    rules: dict[str, Any] = Field(default_factory=dict)
    enabled: bool = True
    created_by: Optional[str] = None


class ApprovalCreate(BaseModel):
    organisation_id: Optional[str] = None
    resource_type: str
    resource_id: Optional[str] = None
    action: str
    requested_by: Optional[str] = None
    approval_chain: list[dict[str, Any]] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)


@router.post("/policies", status_code=201)
async def create_policy(payload: PolicyCreate, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text(
            """
            INSERT INTO governance_policies
              (organisation_id, name, scope, policy_type, rules, enabled, created_by)
            VALUES
              (:organisation_id::uuid, :name, :scope, :policy_type, :rules::jsonb, :enabled, :created_by)
            RETURNING id::text
            """
        ),
        {
            "organisation_id": payload.organisation_id,
            "name": payload.name,
            "scope": payload.scope,
            "policy_type": payload.policy_type,
            "rules": json.dumps(payload.rules),
            "enabled": payload.enabled,
            "created_by": payload.created_by,
        },
    )
    row = result.fetchone()
    return {"id": row[0] if row else ""}


@router.get("/policies")
async def list_policies(
    organisation_id: Optional[str] = None,
    enabled: Optional[bool] = None,
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
):
    clauses = ["1=1"]
    params: dict[str, Any] = {"limit": limit}
    if organisation_id:
        clauses.append("organisation_id=:organisation_id::uuid")
        params["organisation_id"] = organisation_id
    if enabled is not None:
        clauses.append("enabled=:enabled")
        params["enabled"] = enabled
    where = " AND ".join(clauses)
    rows = await db.execute(
        text(
            f"""
            SELECT id::text, organisation_id::text, name, scope, policy_type,
                   rules, enabled, created_by, created_at, updated_at
            FROM governance_policies
            WHERE {where}
            ORDER BY updated_at DESC
            LIMIT :limit
            """
        ),
        params,
    )
    return [dict(r._mapping) for r in rows.fetchall()]


@router.patch("/policies/{policy_id}")
async def update_policy(policy_id: str, patch: dict[str, Any], db: AsyncSession = Depends(get_db)):
    allowed = {"name", "scope", "policy_type", "rules", "enabled"}
    sets = []
    params: dict[str, Any] = {"policy_id": policy_id}
    for key, value in patch.items():
        if key not in allowed:
            continue
        param = f"v_{key}"
        if key == "rules":
            sets.append(f"{key}=:{param}::jsonb")
            params[param] = json.dumps(value)
        else:
            sets.append(f"{key}=:{param}")
            params[param] = value
    if not sets:
        raise HTTPException(status_code=400, detail="No allowed fields")
    sets.append("updated_at=NOW()")
    result = await db.execute(
        text(f"UPDATE governance_policies SET {', '.join(sets)} WHERE id=:policy_id::uuid"),
        params,
    )
    if result.rowcount == 0:
        raise HTTPException(status_code=404, detail="Policy not found")
    return {"status": "ok"}


@router.post("/approvals", status_code=201)
async def request_approval(payload: ApprovalCreate, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text(
            """
            INSERT INTO approval_workflows
              (organisation_id, resource_type, resource_id, action, requested_by, approval_chain, metadata, status)
            VALUES
              (:organisation_id::uuid, :resource_type, :resource_id::uuid, :action, :requested_by, :approval_chain::jsonb, :metadata::jsonb, 'pending')
            RETURNING id::text
            """
        ),
        {
            "organisation_id": payload.organisation_id,
            "resource_type": payload.resource_type,
            "resource_id": payload.resource_id,
            "action": payload.action,
            "requested_by": payload.requested_by,
            "approval_chain": json.dumps(payload.approval_chain),
            "metadata": json.dumps(payload.metadata),
        },
    )
    row = result.fetchone()
    return {"id": row[0] if row else "", "status": "pending"}


@router.post("/approvals/{approval_id}/approve")
async def approve(approval_id: str, approved_by: str, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text(
            """
            UPDATE approval_workflows
            SET status='approved', approved_by=:approved_by, approved_at=NOW(), updated_at=NOW()
            WHERE id=:approval_id::uuid AND status='pending'
            """
        ),
        {"approval_id": approval_id, "approved_by": approved_by},
    )
    if result.rowcount == 0:
        raise HTTPException(status_code=404, detail="Approval not found or already resolved")
    return {"status": "approved"}


@router.get("/approvals")
async def list_approvals(
    organisation_id: Optional[str] = None,
    status: Optional[str] = None,
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
):
    clauses = ["1=1"]
    params: dict[str, Any] = {"limit": limit}
    if organisation_id:
        clauses.append("organisation_id=:organisation_id::uuid")
        params["organisation_id"] = organisation_id
    if status:
        clauses.append("status=:status")
        params["status"] = status
    where = " AND ".join(clauses)
    rows = await db.execute(
        text(
            f"""
            SELECT id::text, organisation_id::text, resource_type, resource_id::text,
                   action, requested_by, approval_chain, status, approved_by,
                   approved_at, metadata, created_at, updated_at
            FROM approval_workflows
            WHERE {where}
            ORDER BY created_at DESC
            LIMIT :limit
            """
        ),
        params,
    )
    return [dict(r._mapping) for r in rows.fetchall()]
