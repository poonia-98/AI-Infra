"""
Control Brain Router — exposes /api/v1/brain/* endpoints
Proxies to the control-brain microservice and returns live data.
"""
from __future__ import annotations
from typing import Optional
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel
from services.brain_service import brain_service

router = APIRouter(prefix="/api/v1/brain", tags=["control-brain"])


# ── Models ──────────────────────────────────────────────────────────────────

class DecisionRequest(BaseModel):
    decision_type: str
    agent_id: Optional[str] = ""
    service_name: Optional[str] = ""
    reason: Optional[str] = ""
    action: Optional[dict] = {}

class ScaleRequest(BaseModel):
    agent_id: str
    direction: str  # up | down
    replicas: Optional[int] = 1
    reason: Optional[str] = ""

class PlacementRequest(BaseModel):
    cpu_request: Optional[float] = 0.5
    memory_request_mb: Optional[float] = 256
    region: Optional[str] = ""


# ── Platform state ────────────────────────────────────────────────────────────

@router.get("/state")
async def get_brain_state():
    """Full platform state from control-brain."""
    return await brain_service.get_state()


@router.get("/state/summary")
async def get_brain_summary():
    """Lightweight status summary."""
    return await brain_service.get_summary()


@router.get("/state/agents")
async def get_brain_agents():
    """All agent states tracked by brain."""
    agents = await brain_service.get_agents()
    return {"agents": agents, "total": len(agents)}


# ── Service registry ──────────────────────────────────────────────────────────

@router.get("/services")
async def get_services():
    """All registered services and their health."""
    return await brain_service.get_services()


# ── Node registry ─────────────────────────────────────────────────────────────

@router.get("/nodes")
async def get_nodes():
    """All registered compute nodes."""
    return await brain_service.get_nodes()


# ── Decisions ─────────────────────────────────────────────────────────────────

@router.get("/decisions")
async def get_decisions():
    """Orchestration decisions log."""
    return await brain_service.get_decisions()


@router.post("/decisions")
async def create_decision(req: DecisionRequest):
    """Issue a manual orchestration decision."""
    ok = await brain_service.issue_decision(
        req.decision_type, req.agent_id, req.service_name, req.reason, req.action
    )
    if not ok:
        raise HTTPException(503, "Brain unavailable — decision could not be queued")
    return {"queued": True, "decision_type": req.decision_type}


# ── Scaling ───────────────────────────────────────────────────────────────────

@router.post("/scale")
async def scale_agent(req: ScaleRequest):
    """Request agent scaling through brain."""
    ok = await brain_service.scale_agent(req.agent_id, req.direction, req.replicas, req.reason)
    if not ok:
        raise HTTPException(503, "Brain unavailable")
    return {"scale_queued": True, "agent_id": req.agent_id, "direction": req.direction}


# ── Placement ─────────────────────────────────────────────────────────────────

@router.post("/placement")
async def get_placement(req: PlacementRequest):
    """Get optimal node placement recommendation."""
    return await brain_service.get_placement(req.cpu_request, req.memory_request_mb, req.region)


# ── Reconcile ─────────────────────────────────────────────────────────────────

@router.post("/reconcile")
async def trigger_reconcile():
    """Trigger an immediate reconciliation cycle."""
    ok = await brain_service.trigger_reconcile()
    return {"triggered": ok}


# ── Health ────────────────────────────────────────────────────────────────────

@router.get("/health")
async def brain_health():
    """Check if control-brain is reachable."""
    healthy = await brain_service.health()
    return {"control_brain": "healthy" if healthy else "unreachable"}