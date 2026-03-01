"""
Cloud platform extension routers:
  /api/v1/observability  - traces, lineage, performance, diagnostics
  /api/v1/sandbox        - sandbox policies and violations
  /api/v1/simulation     - simulation environments and runs
  /api/v1/regions        - multi-region placement
  /api/v1/federation     - federation peers and tasks
  /api/v1/evolution      - agent evolution experiments
  /api/v1/knowledge      - vector knowledge bases
"""
from __future__ import annotations

import httpx
import structlog
from typing import Optional, List
from fastapi import APIRouter, Depends, HTTPException, Query
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from pydantic import BaseModel

from database import get_db
from rbac import require_permission, Permission
from config import settings

logger = structlog.get_logger()

SANDBOX_URL  = getattr(settings, "SANDBOX_URL",  "http://agent-sandbox-manager:8096")
SIM_URL      = getattr(settings, "SIM_URL",      "http://agent-simulation-engine:8097")
GLOBAL_SCHED = getattr(settings, "GLOBAL_SCHED_URL", "http://global-scheduler:8098")
OBS_URL      = getattr(settings, "OBS_URL",      "http://agent-observability:8095")
VECTOR_URL   = getattr(settings, "VECTOR_URL",   "http://memory-vector-engine:8103")

# =============================================================================
#  OBSERVABILITY
# =============================================================================
observability_router = APIRouter(prefix="/api/v1/observability", tags=["observability"])

@observability_router.get("/agents/{agent_id}/traces")
async def get_agent_traces(
    agent_id: str,
    limit: int = Query(20, le=100),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT id::text, execution_id::text, trace_id, root_span_id,
               total_duration_us, cpu_time_us, peak_cpu_pct, peak_memory_mb,
               avg_cpu_pct, avg_memory_mb, created_at
        FROM execution_traces
        WHERE agent_id=:agent_id::uuid
        ORDER BY created_at DESC LIMIT :limit
    """), {"agent_id": agent_id, "limit": limit})
    traces = []
    for r in rows.fetchall():
        traces.append({
            "id": r[0], "execution_id": r[1], "trace_id": r[2], "root_span_id": r[3],
            "total_duration_us": r[4], "cpu_time_us": r[5],
            "peak_cpu_pct": r[6], "peak_memory_mb": r[7],
            "avg_cpu_pct": r[8], "avg_memory_mb": r[9], "created_at": r[10],
        })
    return traces

@observability_router.get("/agents/{agent_id}/lineage")
async def get_agent_lineage(agent_id: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(text("""
        SELECT al.id::text, al.parent_agent_id::text, al.lineage_type,
               al.depth, al.ancestry, a.name as parent_name, al.created_at
        FROM agent_lineage al
        LEFT JOIN agents a ON a.id=al.parent_agent_id
        WHERE al.agent_id=:agent_id::uuid
    """), {"agent_id": agent_id})
    rec = row.fetchone()
    if not rec:
        return {"agent_id": agent_id, "lineage": None}
    return {
        "id": rec[0], "parent_agent_id": rec[1], "lineage_type": rec[2],
        "depth": rec[3], "ancestry": rec[4], "parent_name": rec[5],
        "created_at": rec[6],
    }

@observability_router.get("/agents/{agent_id}/performance")
async def get_performance_snapshots(
    agent_id: str,
    hours: int = Query(24, le=168),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT p50_latency_ms, p90_latency_ms, p99_latency_ms, avg_latency_ms,
               executions_per_min, success_rate, error_rate,
               avg_cpu_pct, avg_memory_pct, window_start, window_end
        FROM performance_snapshots
        WHERE agent_id=:agent_id::uuid
          AND created_at >= NOW() - INTERVAL '1 hour' * :hours
        ORDER BY window_start DESC
    """), {"agent_id": agent_id, "hours": hours})
    snapshots = []
    for r in rows.fetchall():
        snapshots.append({
            "p50_latency_ms": r[0], "p90_latency_ms": r[1], "p99_latency_ms": r[2],
            "avg_latency_ms": r[3], "executions_per_min": r[4],
            "success_rate": r[5], "error_rate": r[6],
            "avg_cpu_pct": r[7], "avg_memory_pct": r[8],
            "window_start": r[9], "window_end": r[10],
        })
    return snapshots

@observability_router.get("/agents/{agent_id}/diagnostics")
async def get_failure_diagnostics(
    agent_id: str,
    limit: int = Query(20, le=100),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT id::text, execution_id::text, failure_class, error_code,
               error_message, auto_recovered, recovery_action,
               remediation, created_at
        FROM failure_diagnostics
        WHERE agent_id=:agent_id::uuid
        ORDER BY created_at DESC LIMIT :limit
    """), {"agent_id": agent_id, "limit": limit})
    diags = []
    for r in rows.fetchall():
        diags.append({
            "id": r[0], "execution_id": r[1], "failure_class": r[2],
            "error_code": r[3], "error_message": r[4],
            "auto_recovered": r[5], "recovery_action": r[6],
            "remediation": r[7] if isinstance(r[7], list) else [],
            "created_at": r[8],
        })
    return diags

@observability_router.get("/executions/{execution_id}/trace")
async def get_execution_trace(execution_id: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(text("""
        SELECT id::text, agent_id::text, trace_id, root_span_id,
               spans, total_duration_us, peak_cpu_pct, peak_memory_mb, tags
        FROM execution_traces WHERE execution_id=:exec_id::uuid
    """), {"exec_id": execution_id})
    rec = row.fetchone()
    if not rec:
        raise HTTPException(404, "Trace not found")
    return {
        "id": rec[0], "agent_id": rec[1], "trace_id": rec[2], "root_span_id": rec[3],
        "spans": rec[4] if isinstance(rec[4], list) else [],
        "total_duration_us": rec[5], "peak_cpu_pct": rec[6],
        "peak_memory_mb": rec[7], "tags": rec[8] if isinstance(rec[8], dict) else {},
    }

# =============================================================================
#  SANDBOX
# =============================================================================
sandbox_router = APIRouter(prefix="/api/v1/sandbox", tags=["sandbox"])

@sandbox_router.get("/policies")
async def list_sandbox_policies(
    org_id: Optional[str] = Query(None),
    db: AsyncSession = Depends(get_db),
):
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(f"{SANDBOX_URL}/policies", params={"organisation_id": org_id or ""})
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass
    rows = await db.execute(text("""
        SELECT id::text, name, runtime, network_mode, max_memory_mb, max_cpu_pct, is_default
        FROM sandbox_policies
        WHERE organisation_id=:org_id::uuid OR organisation_id IS NULL
        ORDER BY (organisation_id IS NOT NULL) DESC
    """), {"org_id": org_id or "00000000-0000-0000-0000-000000000000"})
    return [{"id": r[0], "name": r[1], "runtime": r[2], "network_mode": r[3],
             "max_memory_mb": r[4], "max_cpu_pct": r[5], "is_default": r[6]}
            for r in rows.fetchall()]

@sandbox_router.post("/policies")
async def create_sandbox_policy(body: dict, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.post(f"{SANDBOX_URL}/policies", json=body)
            return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Sandbox manager unavailable: {e}")

@sandbox_router.get("/agents/{agent_id}/violations")
async def get_agent_violations(
    agent_id: str,
    limit: int = Query(50, le=200),
    db: AsyncSession = Depends(get_db),
):
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(f"{SANDBOX_URL}/agents/{agent_id}/violations")
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass
    rows = await db.execute(text("""
        SELECT id::text, violation_type, details, severity, action_taken, created_at
        FROM sandbox_violations WHERE agent_id=:agent_id::uuid
        ORDER BY created_at DESC LIMIT :limit
    """), {"agent_id": agent_id, "limit": limit})
    return [{"id": r[0], "violation_type": r[1], "details": r[2],
             "severity": r[3], "action_taken": r[4], "created_at": r[5]}
            for r in rows.fetchall()]

@sandbox_router.get("/runtimes")
async def list_runtimes():
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(f"{SANDBOX_URL}/runtimes")
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass
    return [{"name": "docker", "available": True, "isolation": "container"}]

# =============================================================================
#  SIMULATION
# =============================================================================
simulation_router = APIRouter(prefix="/api/v1/simulation", tags=["simulation"])

@simulation_router.get("/environments")
async def list_environments(
    org_id: str = Query(...),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT id::text, name, description, environment_type, status, created_at
        FROM simulation_environments WHERE organisation_id=:org_id::uuid
        ORDER BY created_at DESC
    """), {"org_id": org_id})
    return [{"id": r[0], "name": r[1], "description": r[2],
             "environment_type": r[3], "status": r[4], "created_at": r[5]}
            for r in rows.fetchall()]

@simulation_router.post("/environments")
async def create_environment(body: dict, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.post(f"{SIM_URL}/environments", json=body)
            return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Simulation engine unavailable: {e}")

@simulation_router.post("/runs")
async def queue_simulation_run(body: dict, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.post(f"{SIM_URL}/runs", json=body)
            return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Simulation engine unavailable: {e}")

@simulation_router.get("/runs/{run_id}")
async def get_simulation_run(run_id: str, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(f"{SIM_URL}/runs/{run_id}")
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass
    row = await db.execute(text("""
        SELECT id::text, agent_id::text, run_type, status,
               results, verdict, error, duration_ms, created_at, completed_at
        FROM simulation_runs WHERE id=:run_id::uuid
    """), {"run_id": run_id})
    rec = row.fetchone()
    if not rec:
        raise HTTPException(404, "Run not found")
    return {
        "id": rec[0], "agent_id": rec[1], "run_type": rec[2], "status": rec[3],
        "results": rec[4], "verdict": rec[5], "error": rec[6],
        "duration_ms": rec[7], "created_at": rec[8], "completed_at": rec[9],
    }

@simulation_router.get("/agents/{agent_id}/runs")
async def list_agent_simulation_runs(
    agent_id: str,
    limit: int = Query(20, le=100),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT id::text, run_type, status, verdict, assertions_passed,
               assertions_total, duration_ms, created_at
        FROM simulation_runs WHERE agent_id=:agent_id::uuid
        ORDER BY created_at DESC LIMIT :limit
    """), {"agent_id": agent_id, "limit": limit})
    return [{"id": r[0], "run_type": r[1], "status": r[2], "verdict": r[3],
             "assertions_passed": r[4], "assertions_total": r[5],
             "duration_ms": r[6], "created_at": r[7]}
            for r in rows.fetchall()]

# =============================================================================
#  MULTI-REGION
# =============================================================================
regions_router = APIRouter(prefix="/api/v1/regions", tags=["regions"])

@regions_router.get("")
async def list_regions(db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.get(f"{GLOBAL_SCHED}/regions")
            if resp.status_code == 200:
                return resp.json()
    except Exception:
        pass
    rows = await db.execute(text("""
        SELECT r.id::text, r.name, r.display_name, r.provider, r.status,
               r.last_heartbeat, r.latency_ms, r.supports_k8s,
               COUNT(c.id) as clusters
        FROM regions r LEFT JOIN clusters c ON c.region_id=r.id
        GROUP BY r.id ORDER BY r.name
    """))
    return [{"id": r[0], "name": r[1], "display_name": r[2], "provider": r[3],
             "status": r[4], "last_heartbeat": r[5], "latency_ms": r[6],
             "supports_k8s": r[7], "clusters": r[8]}
            for r in rows.fetchall()]

@regions_router.post("")
async def register_region(body: dict, db: AsyncSession = Depends(get_db),
                           _=Depends(require_permission(Permission.ADMIN_ACCESS))):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.post(f"{GLOBAL_SCHED}/regions", json=body)
            return resp.json()
    except Exception:
        pass
    result = await db.execute(text("""
        INSERT INTO regions (name, display_name, provider, endpoint, supports_k8s)
        VALUES (:name, :display_name, :provider, :endpoint, :k8s)
        ON CONFLICT (name) DO UPDATE SET endpoint=EXCLUDED.endpoint, updated_at=NOW()
        RETURNING id::text
    """), {
        "name": body.get("name"), "display_name": body.get("display_name", body.get("name")),
        "provider": body.get("provider", "self-hosted"),
        "endpoint": body.get("endpoint", ""),
        "k8s": body.get("supports_k8s", False),
    })
    row = result.fetchone()
    await db.commit()
    return {"id": row[0], "name": body.get("name")}

@regions_router.get("/placements/{agent_id}")
async def get_agent_placements(agent_id: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT gp.id::text, r.name, r.status as region_status,
               gp.placement_type, gp.status, gp.scheduled_at
        FROM global_placements gp JOIN regions r ON r.id=gp.region_id
        WHERE gp.agent_id=:agent_id::uuid ORDER BY gp.created_at DESC
    """), {"agent_id": agent_id})
    return [{"id": r[0], "region": r[1], "region_status": r[2],
             "placement_type": r[3], "status": r[4], "scheduled_at": r[5]}
            for r in rows.fetchall()]

@regions_router.post("/placements")
async def create_placement(body: dict, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=5) as client:
            resp = await client.post(f"{GLOBAL_SCHED}/placements", json=body)
            return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Global scheduler unavailable: {e}")

@regions_router.get("/failovers")
async def list_failover_events(
    limit: int = Query(20, le=100),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT rfe.id::text, src.name as source_region, tgt.name as target_region,
               rfe.trigger_reason, rfe.agents_migrated, rfe.status, rfe.created_at
        FROM region_failover_events rfe
        JOIN regions src ON src.id=rfe.source_region_id
        JOIN regions tgt ON tgt.id=rfe.target_region_id
        ORDER BY rfe.created_at DESC LIMIT :limit
    """), {"limit": limit})
    return [{"id": r[0], "source_region": r[1], "target_region": r[2],
             "trigger_reason": r[3], "agents_migrated": r[4],
             "status": r[5], "created_at": r[6]}
            for r in rows.fetchall()]

# =============================================================================
#  FEDERATION
# =============================================================================
federation_router = APIRouter(prefix="/api/v1/federation", tags=["federation"])

@federation_router.get("/peers/{org_id}")
async def list_federation_peers(org_id: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT id::text, peer_name, peer_endpoint, status, trust_level,
               capabilities, max_rps, created_at
        FROM federation_peers WHERE owner_org_id=:org_id::uuid
        ORDER BY created_at DESC
    """), {"org_id": org_id})
    return [{"id": r[0], "peer_name": r[1], "peer_endpoint": r[2],
             "status": r[3], "trust_level": r[4],
             "capabilities": r[5] if isinstance(r[5], list) else [],
             "max_rps": r[6], "created_at": r[7]}
            for r in rows.fetchall()]

@federation_router.post("/peers/{org_id}")
async def create_federation_peer(
    org_id: str,
    body: dict,
    db: AsyncSession = Depends(get_db),
    _=Depends(require_permission(Permission.ADMIN_ACCESS)),
):
    import json
    caps = json.dumps(body.get("capabilities", ["agent_messaging", "task_dispatch"]))
    result = await db.execute(text("""
        INSERT INTO federation_peers
          (owner_org_id, peer_name, peer_endpoint, trust_level, capabilities, max_rps)
        VALUES (:org_id::uuid, :name, :endpoint, :trust, :caps::text[], :max_rps)
        RETURNING id::text
    """), {
        "org_id": org_id,
        "name": body.get("peer_name"),
        "endpoint": body.get("peer_endpoint"),
        "trust": body.get("trust_level", "limited"),
        "caps": caps,
        "max_rps": body.get("max_rps", 100),
    })
    row = result.fetchone()
    await db.commit()
    return {"id": row[0], "status": "pending"}

@federation_router.patch("/peers/{org_id}/{peer_id}/activate")
async def activate_federation_peer(
    org_id: str,
    peer_id: str,
    db: AsyncSession = Depends(get_db),
    _=Depends(require_permission(Permission.ADMIN_ACCESS)),
):
    await db.execute(text("""
        UPDATE federation_peers SET status='active', updated_at=NOW()
        WHERE id=:peer_id::uuid AND owner_org_id=:org_id::uuid
    """), {"peer_id": peer_id, "org_id": org_id})
    await db.commit()
    return {"status": "active"}

@federation_router.get("/tasks/{org_id}")
async def list_federated_tasks(
    org_id: str,
    status: Optional[str] = None,
    limit: int = Query(50, le=200),
    db: AsyncSession = Depends(get_db),
):
    conditions = ["fp.owner_org_id=:org_id::uuid"]
    params: dict = {"org_id": org_id, "limit": limit}
    if status:
        conditions.append("ft.status=:status")
        params["status"] = status
    where = " AND ".join(conditions)
    rows = await db.execute(text(f"""
        SELECT ft.id::text, fp.peer_name, ft.origin, ft.target_agent_id,
               ft.task_type, ft.status, ft.created_at, ft.completed_at
        FROM federated_tasks ft JOIN federation_peers fp ON fp.id=ft.peer_id
        WHERE {where}
        ORDER BY ft.created_at DESC LIMIT :limit
    """), params)
    return [{"id": r[0], "peer_name": r[1], "origin": r[2], "target_agent_id": r[3],
             "task_type": r[4], "status": r[5], "created_at": r[6], "completed_at": r[7]}
            for r in rows.fetchall()]

@federation_router.post("/tasks")
async def dispatch_federated_task(
    body: dict,
    db: AsyncSession = Depends(get_db),
):
    import json
    payload = json.dumps(body.get("payload", {}))
    result = await db.execute(text("""
        INSERT INTO federated_tasks
          (peer_id, origin, source_agent_id, target_agent_id, task_type, payload, status)
        VALUES (:peer_id::uuid, 'outbound', NULLIF(:src,'')::uuid, :target, :task_type, :payload::jsonb, 'pending')
        RETURNING id::text
    """), {
        "peer_id": body.get("peer_id"),
        "src": body.get("source_agent_id", ""),
        "target": body.get("target_agent_id"),
        "task_type": body.get("task_type", "execute"),
        "payload": payload,
    })
    row = result.fetchone()
    await db.commit()
    return {"task_id": row[0], "status": "pending"}

# =============================================================================
#  EVOLUTION ENGINE
# =============================================================================
evolution_router = APIRouter(prefix="/api/v1/evolution", tags=["evolution"])

@evolution_router.get("/experiments/{org_id}")
async def list_experiments(org_id: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT ee.id::text, ee.name, ee.strategy, a.name as base_agent,
               ee.status, ee.current_generation, ee.max_generations,
               ee.best_fitness, ee.population_size, ee.created_at
        FROM evolution_experiments ee JOIN agents a ON a.id=ee.base_agent_id
        WHERE ee.organisation_id=:org_id::uuid ORDER BY ee.created_at DESC
    """), {"org_id": org_id})
    return [{"id": r[0], "name": r[1], "strategy": r[2], "base_agent": r[3],
             "status": r[4], "current_generation": r[5], "max_generations": r[6],
             "best_fitness": r[7], "population_size": r[8], "created_at": r[9]}
            for r in rows.fetchall()]

@evolution_router.post("/experiments/{org_id}")
async def create_experiment(
    org_id: str,
    body: dict,
    db: AsyncSession = Depends(get_db),
):
    import json
    cfg_json = json.dumps(body.get("config", {}))
    ff_json = json.dumps(body.get("fitness_function", {}))
    result = await db.execute(text("""
        INSERT INTO evolution_experiments
          (organisation_id, name, description, strategy, base_agent_id,
           config, population_size, max_generations, fitness_function,
           mutation_rate, crossover_rate, selection_pressure, status)
        VALUES (:org_id::uuid, :name, :desc, :strategy, :agent_id::uuid,
                :config::jsonb, :pop, :max_gen, :ff::jsonb,
                :mut_rate, :cross_rate, :sel_pressure, 'running')
        RETURNING id::text
    """), {
        "org_id": org_id, "name": body.get("name"),
        "desc": body.get("description", ""),
        "strategy": body.get("strategy", "genetic"),
        "agent_id": body.get("base_agent_id"),
        "config": cfg_json,
        "pop": body.get("population_size", 10),
        "max_gen": body.get("max_generations", 20),
        "ff": ff_json,
        "mut_rate": body.get("mutation_rate", 0.1),
        "cross_rate": body.get("crossover_rate", 0.7),
        "sel_pressure": body.get("selection_pressure", 0.5),
    })
    row = result.fetchone()
    await db.commit()
    return {"experiment_id": row[0], "status": "running"}

@evolution_router.get("/agents/{agent_id}/genome")
async def get_agent_genome(agent_id: str, db: AsyncSession = Depends(get_db)):
    row = await db.execute(text("""
        SELECT id::text, genome_version, traits, fitness_scores, overall_fitness,
               parent_genome_id::text, mutation_log, status, created_at
        FROM agent_genomes WHERE agent_id=:agent_id::uuid
        ORDER BY genome_version DESC LIMIT 1
    """), {"agent_id": agent_id})
    rec = row.fetchone()
    if not rec:
        raise HTTPException(404, "No genome found for agent")
    return {
        "id": rec[0], "version": rec[1],
        "traits": rec[2] if isinstance(rec[2], dict) else {},
        "fitness_scores": rec[3] if isinstance(rec[3], dict) else {},
        "overall_fitness": rec[4], "parent_genome_id": rec[5],
        "mutation_log": rec[6] if isinstance(rec[6], list) else [],
        "status": rec[7], "created_at": rec[8],
    }

@evolution_router.get("/agents/{agent_id}/clones")
async def list_agent_clones(agent_id: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT ca.id::text, a.name, a.status, ca.clone_type,
               ca.generation, ca.fitness_score, ca.created_at
        FROM cloned_agents ca JOIN agents a ON a.id=ca.clone_agent_id
        WHERE ca.source_agent_id=:agent_id::uuid
        ORDER BY ca.created_at DESC
    """), {"agent_id": agent_id})
    return [{"id": r[0], "name": r[1], "status": r[2], "clone_type": r[3],
             "generation": r[4], "fitness_score": r[5], "created_at": r[6]}
            for r in rows.fetchall()]

# =============================================================================
#  VECTOR KNOWLEDGE BASES
# =============================================================================
knowledge_router = APIRouter(prefix="/api/v1/knowledge", tags=["knowledge"])

@knowledge_router.get("/bases/{org_id}")
async def list_knowledge_bases(org_id: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("""
        SELECT id::text, name, description, embedding_model, embedding_dim,
               chunk_strategy, total_chunks, is_shared, created_at
        FROM knowledge_bases WHERE organisation_id=:org_id::uuid
        ORDER BY created_at DESC
    """), {"org_id": org_id})
    return [{"id": r[0], "name": r[1], "description": r[2], "embedding_model": r[3],
             "embedding_dim": r[4], "chunk_strategy": r[5], "total_chunks": r[6],
             "is_shared": r[7], "created_at": r[8]}
            for r in rows.fetchall()]

@knowledge_router.post("/bases/{org_id}")
async def create_knowledge_base(org_id: str, body: dict, db: AsyncSession = Depends(get_db)):
    result = await db.execute(text("""
        INSERT INTO knowledge_bases
          (organisation_id, name, description, embedding_model, embedding_dim,
           chunk_strategy, chunk_size, chunk_overlap, is_shared)
        VALUES (:org_id::uuid, :name, :desc, :model, :dim, :strategy, :size, :overlap, :shared)
        RETURNING id::text
    """), {
        "org_id": org_id, "name": body.get("name"),
        "desc": body.get("description", ""),
        "model": body.get("embedding_model", "text-embedding-ada-002"),
        "dim": body.get("embedding_dim", 1536),
        "strategy": body.get("chunk_strategy", "paragraph"),
        "size": body.get("chunk_size", 512),
        "overlap": body.get("chunk_overlap", 64),
        "shared": body.get("is_shared", False),
    })
    row = result.fetchone()
    await db.commit()
    return {"id": row[0], "name": body.get("name")}

@knowledge_router.post("/bases/{kb_id}/search")
async def semantic_search(kb_id: str, body: dict, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            resp = await client.post(f"{VECTOR_URL}/knowledge/{kb_id}/search", json=body)
            if resp.status_code == 200:
                return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Vector engine unavailable: {e}")

@knowledge_router.post("/bases/{kb_id}/chunks")
async def add_chunk(kb_id: str, body: dict, db: AsyncSession = Depends(get_db)):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            body["knowledge_base_id"] = kb_id
            resp = await client.post(f"{VECTOR_URL}/chunks", json=body)
            if resp.status_code in (200, 201):
                return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Vector engine unavailable: {e}")

@knowledge_router.get("/bases/{kb_id}/chunks")
async def list_chunks(
    kb_id: str,
    limit: int = Query(50, le=200),
    db: AsyncSession = Depends(get_db),
):
    rows = await db.execute(text("""
        SELECT id::text, source_type, source_id, content, chunk_index, token_count, created_at
        FROM knowledge_chunks WHERE knowledge_base_id=:kb_id::uuid
        ORDER BY chunk_index LIMIT :limit
    """), {"kb_id": kb_id, "limit": limit})
    return [{"id": r[0], "source_type": r[1], "source_id": r[2], "content": r[3],
             "chunk_index": r[4], "token_count": r[5], "created_at": r[6]}
            for r in rows.fetchall()]

# =============================================================================
#  VAULT SECRETS ROUTER (extends existing secrets)
# =============================================================================
vault_router = APIRouter(prefix="/api/v1/vault", tags=["vault"])

@vault_router.get("/{org_id}/secrets")
async def list_vault_secrets(org_id: str, db: AsyncSession = Depends(get_db),
                              _=Depends(require_permission(Permission.ADMIN_ACCESS))):
    rows = await db.execute(text("""
        SELECT id::text, secret_type, name, path, description,
               auto_rotate, current_version, status, last_rotated, created_at
        FROM vault_secrets WHERE organisation_id=:org_id::uuid
        ORDER BY path
    """), {"org_id": org_id})
    return [{"id": r[0], "secret_type": r[1], "name": r[2], "path": r[3],
             "description": r[4], "auto_rotate": r[5], "current_version": r[6],
             "status": r[7], "last_rotated": r[8], "created_at": r[9]}
            for r in rows.fetchall()]

@vault_router.post("/{org_id}/secrets")
async def create_vault_secret(
    org_id: str,
    body: dict,
    db: AsyncSession = Depends(get_db),
    _=Depends(require_permission(Permission.ADMIN_ACCESS)),
):
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            body["organisation_id"] = org_id
            resp = await client.post(
                f"{getattr(settings, 'SECRETS_MANAGER_URL', 'http://secrets-manager:8104')}/secrets",
                json=body,
            )
            return resp.json()
    except Exception as e:
        raise HTTPException(503, f"Secrets manager unavailable: {e}")

@vault_router.get("/{org_id}/secrets/{secret_path:path}/access-log")
async def get_secret_access_log(
    org_id: str,
    secret_path: str,
    limit: int = Query(50, le=200),
    db: AsyncSession = Depends(get_db),
    _=Depends(require_permission(Permission.ADMIN_ACCESS)),
):
    rows = await db.execute(text("""
        SELECT sal.id::text, sal.accessor_type, sal.accessor_id, sal.action,
               sal.success, sal.ip_address::text, sal.accessed_at
        FROM secret_access_logs sal
        JOIN vault_secrets vs ON vs.id=sal.secret_id
        WHERE vs.organisation_id=:org_id::uuid AND vs.path=:path
        ORDER BY sal.accessed_at DESC LIMIT :limit
    """), {"org_id": org_id, "path": "/" + secret_path.lstrip("/"), "limit": limit})
    return [{"id": r[0], "accessor_type": r[1], "accessor_id": r[2], "action": r[3],
             "success": r[4], "ip_address": r[5], "accessed_at": r[6]}
            for r in rows.fetchall()]