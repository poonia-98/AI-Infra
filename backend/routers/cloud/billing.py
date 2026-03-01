"""
Billing API
  /api/v1/billing/plans          - list/get plans
  /api/v1/billing/subscriptions  - manage subscriptions
  /api/v1/billing/invoices       - list/get invoices
  /api/v1/billing/payments       - payment records
  /api/v1/billing/usage          - usage queries
  /api/v1/billing/cost-estimate  - realtime cost estimate
"""
from __future__ import annotations

import uuid
from typing import Optional
from datetime import datetime, timezone, date

import httpx
import structlog
from fastapi import APIRouter, Depends, HTTPException, Query, BackgroundTasks
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from pydantic import BaseModel

from database import get_db
from rbac import require_permission, Permission
from config import settings

logger = structlog.get_logger()
billing_router = APIRouter(prefix="/api/v1/billing", tags=["billing"])

USAGE_METER_URL = getattr(settings, "USAGE_METER_URL", "http://usage-meter:8100")
BILLING_ENGINE_URL = getattr(settings, "BILLING_ENGINE_URL", "http://billing-engine:8101")
SUBSCRIPTION_MGR_URL = getattr(settings, "SUBSCRIPTION_MGR_URL", "http://subscription-manager:8102")

# ── Plans ──────────────────────────────────────────────────────────────────────

@billing_router.get("/plans")
async def list_plans(db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT id::text, name, display_name, description,
               base_price_cents, price_per_agent_hour, price_per_exec,
               price_per_workflow_exec, price_per_api_call,
               free_agent_hours, free_executions, free_api_calls,
               max_agents, max_executions_month, features, sort_order
        FROM billing_plans WHERE is_active=TRUE AND is_public=TRUE
        ORDER BY sort_order
    """))
    plans = []
    for row in rows.fetchall():
        plans.append({
            "id": row[0], "name": row[1], "display_name": row[2],
            "description": row[3], "base_price_cents": row[4],
            "price_per_agent_hour": row[5], "price_per_exec": row[6],
            "price_per_workflow_exec": row[7], "price_per_api_call": row[8],
            "free_agent_hours": row[9], "free_executions": row[10],
            "free_api_calls": row[11], "max_agents": row[12],
            "max_executions_month": row[13],
            "features": row[14] if isinstance(row[14], dict) else {},
            "sort_order": row[15],
        })
    return plans

@billing_router.get("/plans/{plan_name}")
async def get_plan(plan_name: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(text("""
        SELECT id::text, name, display_name, description, base_price_cents,
               price_per_agent_hour, price_per_exec, price_per_workflow_exec,
               price_per_api_call, free_agent_hours, free_executions,
               free_api_calls, max_agents, features
        FROM billing_plans WHERE name=:name AND is_active=TRUE
    """), {"name": plan_name})
    rec = row.fetchone()
    if not rec:
        raise HTTPException(404, "Plan not found")
    return {
        "id": rec[0], "name": rec[1], "display_name": rec[2],
        "description": rec[3], "base_price_cents": rec[4],
        "price_per_agent_hour": rec[5], "price_per_exec": rec[6],
        "price_per_workflow_exec": rec[7], "price_per_api_call": rec[8],
        "free_agent_hours": rec[9], "free_executions": rec[10],
        "free_api_calls": rec[11], "max_agents": rec[12],
        "features": rec[13] if isinstance(rec[13], dict) else {},
    }

# ── Subscriptions ─────────────────────────────────────────────────────────────

class SubscriptionCreate(BaseModel):
    plan_name: str
    billing_cycle: str = "monthly"
    payment_method_id: Optional[str] = None
    payment_provider: str = "stripe"

@billing_router.get("/subscriptions/{org_id}")
async def get_subscription(org_id: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(text("""
        SELECT s.id::text, bp.name, bp.display_name, s.status,
               s.billing_cycle, s.current_period_start, s.current_period_end,
               s.trial_ends_at, s.cancelled_at, s.external_id
        FROM subscriptions s JOIN billing_plans bp ON bp.id=s.plan_id
        WHERE s.organisation_id=:org_id::uuid
    """), {"org_id": org_id})
    rec = row.fetchone()
    if not rec:
        # Return free plan as default
        return {"plan_name": "free", "status": "active", "billing_cycle": "monthly"}
    return {
        "id": rec[0], "plan_name": rec[1], "display_name": rec[2],
        "status": rec[3], "billing_cycle": rec[4],
        "current_period_start": rec[5], "current_period_end": rec[6],
        "trial_ends_at": rec[7], "cancelled_at": rec[8], "external_id": rec[9],
    }

@billing_router.post("/subscriptions/{org_id}")
async def create_or_update_subscription(
    org_id: str,
    body: SubscriptionCreate,
    db: AsyncSession = Depends(get_db),
    _=Depends(require_permission(Permission.ADMIN_ACCESS)),
):
    # Get plan_id
    row = await db.execute(text("SELECT id FROM billing_plans WHERE name=:name"), {"name": body.plan_name})
    plan = row.fetchone()
    if not plan:
        raise HTTPException(404, f"Plan '{body.plan_name}' not found")

    result = await db.execute(text("""
        INSERT INTO subscriptions (organisation_id, plan_id, status, billing_cycle,
                                   current_period_start, current_period_end,
                                   payment_method_id, payment_provider)
        VALUES (:org_id::uuid, :plan_id::uuid, 'active', :cycle, NOW(),
                NOW() + INTERVAL '1 month', :pm_id, :provider)
        ON CONFLICT (organisation_id) DO UPDATE
          SET plan_id=EXCLUDED.plan_id, billing_cycle=EXCLUDED.billing_cycle,
              status='active', updated_at=NOW()
        RETURNING id::text
    """), {
        "org_id": org_id, "plan_id": str(plan[0]),
        "cycle": body.billing_cycle,
        "pm_id": body.payment_method_id,
        "provider": body.payment_provider,
    })
    sub = result.fetchone()
    await db.commit()
    return {"id": sub[0], "org_id": org_id, "plan_name": body.plan_name, "status": "active"}

@billing_router.delete("/subscriptions/{org_id}")
async def cancel_subscription(
    org_id: str,
    reason: Optional[str] = None,
    db: AsyncSession = Depends(get_db),
    _=Depends(require_permission(Permission.ADMIN_ACCESS)),
):
    await db.execute(text("""
        UPDATE subscriptions
        SET status='cancelled', cancelled_at=NOW(), cancel_reason=:reason, updated_at=NOW()
        WHERE organisation_id=:org_id::uuid
    """), {"org_id": org_id, "reason": reason})
    await db.commit()
    return {"status": "cancelled"}

# ── Invoices ──────────────────────────────────────────────────────────────────

@billing_router.get("/invoices/{org_id}")
async def list_invoices(
    org_id: str,
    limit: int = Query(20, le=100),
    offset: int = 0,
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT id::text, invoice_number, status, period_start, period_end,
               subtotal_cents, discount_cents, tax_cents, total_cents,
               paid_cents, currency, due_at, paid_at, created_at
        FROM invoices WHERE organisation_id=:org_id::uuid
        ORDER BY created_at DESC LIMIT :limit OFFSET :offset
    """), {"org_id": org_id, "limit": limit, "offset": offset})
    invoices = []
    for row in rows.fetchall():
        invoices.append({
            "id": row[0], "invoice_number": row[1], "status": row[2],
            "period_start": row[3], "period_end": row[4],
            "subtotal_cents": row[5], "discount_cents": row[6],
            "tax_cents": row[7], "total_cents": row[8],
            "paid_cents": row[9], "currency": row[10],
            "due_at": row[11], "paid_at": row[12], "created_at": row[13],
        })
    return invoices

@billing_router.get("/invoices/{org_id}/{invoice_id}")
async def get_invoice(org_id: str, invoice_id: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(text("""
        SELECT id::text, invoice_number, status, period_start, period_end,
               subtotal_cents, total_cents, paid_cents, currency,
               line_items, due_at, paid_at, pdf_url
        FROM invoices WHERE id=:inv_id::uuid AND organisation_id=:org_id::uuid
    """), {"inv_id": invoice_id, "org_id": org_id})
    rec = row.fetchone()
    if not rec:
        raise HTTPException(404, "Invoice not found")
    return {
        "id": rec[0], "invoice_number": rec[1], "status": rec[2],
        "period_start": rec[3], "period_end": rec[4],
        "subtotal_cents": rec[5], "total_cents": rec[6],
        "paid_cents": rec[7], "currency": rec[8],
        "line_items": rec[9] if isinstance(rec[9], list) else [],
        "due_at": rec[10], "paid_at": rec[11], "pdf_url": rec[12],
    }

# ── Usage ─────────────────────────────────────────────────────────────────────

@billing_router.get("/usage/{org_id}")
async def get_usage(
    org_id: str,
    since: Optional[str] = None,
    until: Optional[str] = None,
    resource_type: Optional[str] = None,
    db: AsyncSession = Depends(get_db),
):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            params = {"since": since or "", "until": until or ""}
            if resource_type:
                params["resource_type"] = resource_type
            resp = await client.get(f"{USAGE_METER_URL}/orgs/{org_id}/usage", params=params)
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass

    # Fallback: query directly
    conditions = ["organisation_id=:org_id::uuid"]
    params: dict = {"org_id": org_id, "limit": 100}
    if since:
        conditions.append("recorded_at >= :since::timestamptz")
        params["since"] = since
    if until:
        conditions.append("recorded_at <= :until::timestamptz")
        params["until"] = until
    if resource_type:
        conditions.append("resource_type=:resource_type")
        params["resource_type"] = resource_type

    where = " AND ".join(conditions)
    rows = await db.execute(text(f"""
        SELECT resource_type, SUM(quantity) as total, unit
        FROM usage_records WHERE {where}
        GROUP BY resource_type, unit ORDER BY resource_type
    """), params)
    usage = [{"resource_type": r[0], "quantity": r[1], "unit": r[2]} for r in rows.fetchall()]
    return {"organisation_id": org_id, "usage": usage}

@billing_router.get("/cost-estimate/{org_id}")
async def get_cost_estimate(org_id: str, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.get(f"{USAGE_METER_URL}/orgs/{org_id}/cost-estimate")
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass
    return {"organisation_id": org_id, "message": "usage meter unavailable"}

# ── Credits ───────────────────────────────────────────────────────────────────

@billing_router.get("/credits/{org_id}")
async def get_credits(org_id: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT id::text, amount_cents, used_cents, reason, expires_at, created_at
        FROM billing_credits WHERE organisation_id=:org_id::uuid
        ORDER BY created_at DESC
    """), {"org_id": org_id})
    credits = []
    for row in rows.fetchall():
        credits.append({
            "id": row[0], "amount_cents": row[1], "used_cents": row[2],
            "available_cents": row[1] - row[2],
            "reason": row[3], "expires_at": row[4], "created_at": row[5],
        })
    return credits

@billing_router.post("/credits/{org_id}", dependencies=[Depends(require_permission(Permission.ADMIN_ACCESS))])
async def add_credit(
    org_id: str,
    amount_cents: int = Query(...),
    reason: Optional[str] = None,
    db: AsyncSession = Depends(get_db),
):
    result = await db.execute(text("""
        INSERT INTO billing_credits (organisation_id, amount_cents, reason)
        VALUES (:org_id::uuid, :amount, :reason)
        RETURNING id::text
    """), {"org_id": org_id, "amount": amount_cents, "reason": reason})
    row = result.fetchone()
    await db.commit()
    return {"id": row[0], "amount_cents": amount_cents, "status": "applied"}