#!/usr/bin/env python3
"""
agentplane — World's First AI Agent Operating System CLI

Usage:
  agentplane install       Bootstrap platform from scratch (first run)
  agentplane up            Start all services
  agentplane down          Stop all services
  agentplane restart       Restart all services
  agentplane status        Live health dashboard of all services
  agentplane logs [svc]    Tail logs (all services or specific one)
  agentplane migrate       Run all database migrations
  agentplane deploy-agent  Interactive agent deployment wizard
  agentplane reset         Wipe all data and restart fresh (destructive!)
  agentplane dashboard     Open control plane in browser
  agentplane brain         Show control brain state
  agentplane services      List all registered microservices
  agentplane version       Version information

Examples:
  agentplane install
  agentplane up
  agentplane status
  agentplane logs backend
  agentplane deploy-agent my-research-agent agent-runtime:latest
"""

from __future__ import annotations

import json
import os
import platform
import re
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
import webbrowser
from pathlib import Path
from typing import Optional


# ── Constants ──────────────────────────────────────────────────────────────────

VERSION           = "3.0.0"
PLATFORM_NAME     = "AgentPlane"
COMPOSE_FILE      = "docker-compose.yml"
API_URL           = "http://localhost:8000"
BRAIN_URL         = "http://localhost:8200"
DASHBOARD_URL     = "http://localhost:3000"
GRAFANA_URL       = "http://localhost:3001"

CORE_SERVICES = [
    "postgres", "redis", "nats",
]

PLATFORM_SERVICES = [
    "backend", "frontend",
    "executor", "event-processor", "metrics-collector",
    "websocket-gateway", "log-processor", "reconciliation-service",
    "scheduler-service", "node-manager",
    "workflow-engine", "agent-autoscaler", "agent-gateway",
    "marketplace-service", "agent-observability",
    "agent-sandbox-manager", "billing-engine",
    "subscription-manager", "usage-meter",
    "agent-simulation-engine", "agent-evolution-engine",
    "global-scheduler", "region-controller",
    "cluster-controller", "agent-federation-network",
    "memory-vector-engine", "secrets-manager",
    "marketplace-validator", "control-brain",
]

MONITORING_SERVICES = ["prometheus", "grafana", "otel-collector"]

ALL_SERVICES = CORE_SERVICES + PLATFORM_SERVICES + MONITORING_SERVICES

# Health check endpoints per service
HEALTH_MAP = {
    "backend":                  f"{API_URL}/health",
    "control-brain":            f"{BRAIN_URL}/health",
    "memory-vector-engine":     "http://localhost:8103/health",
    "agent-federation-network": "http://localhost:8107/health",
    "agent-simulation-engine":  "http://localhost:8097/health",
    "agent-evolution-engine":   "http://localhost:8106/health",
    "agent-sandbox-manager":    "http://localhost:8096/health",
    "billing-engine":           "http://localhost:8101/health",
    "secrets-manager":          "http://localhost:8104/health",
    "executor":                 "http://localhost:8081/health",
    "region-controller":        "http://localhost:8099/health",
    "global-scheduler":         "http://localhost:8098/health",
    "cluster-controller":       "http://localhost:8108/health",
    "grafana":                  "http://localhost:3001/api/health",
    "prometheus":               "http://localhost:9090/-/ready",
}

# ── Terminal Colors ────────────────────────────────────────────────────────────

CYAN    = "\033[96m"
GREEN   = "\033[92m"
YELLOW  = "\033[93m"
RED     = "\033[91m"
MAGENTA = "\033[95m"
BOLD    = "\033[1m"
DIM     = "\033[2m"
RESET   = "\033[0m"

def c(color: str, text: str) -> str:
    return f"{color}{text}{RESET}"


# ── Logging ───────────────────────────────────────────────────────────────────

def log(msg: str, level: str = "info"):
    prefix = {
        "info":    f"  {c(CYAN, '▶')}",
        "ok":      f"  {c(GREEN, '✓')}",
        "warn":    f"  {c(YELLOW, '⚠')}",
        "error":   f"  {c(RED, '✗')}",
        "section": f"\n{c(BOLD+CYAN, '━━')}",
        "dim":     f"  {c(DIM, '·')}",
    }[level]
    print(f"{prefix} {msg}")


def section(title: str):
    width = 60
    print(f"\n{c(BOLD+CYAN, '┌' + '─' * (width-2) + '┐')}")
    pad = (width - 2 - len(title)) // 2
    print(f"{c(BOLD+CYAN, '│')} {' ' * pad}{c(BOLD, title)}{' ' * (width-4-len(title)-pad)} {c(BOLD+CYAN, '│')}")
    print(f"{c(BOLD+CYAN, '└' + '─' * (width-2) + '┘')}")


def banner():
    print(f"""
{c(BOLD+CYAN, '    █████╗  ██████╗ ███████╗███╗   ██╗████████╗')}
{c(BOLD+CYAN, '   ██╔══██╗██╔════╝ ██╔════╝████╗  ██║╚══██╔══╝')}
{c(BOLD+CYAN, '   ███████║██║  ███╗█████╗  ██╔██╗ ██║   ██║   ')}
{c(BOLD+CYAN, '   ██╔══██║██║   ██║██╔══╝  ██║╚██╗██║   ██║   ')}
{c(BOLD+CYAN, '   ██║  ██║╚██████╔╝███████╗██║ ╚████║   ██║   ')}
{c(BOLD+CYAN, '   ╚═╝  ╚═╝ ╚═════╝ ╚══════╝╚═╝  ╚═══╝   ╚═╝   ')}
{c(DIM,       '          ██████╗ ██╗      █████╗ ███╗   ██╗███████╗')}
{c(DIM,       '          ██╔══██╗██║     ██╔══██╗████╗  ██║██╔════╝')}
{c(DIM,       '          ██████╔╝██║     ███████║██╔██╗ ██║█████╗  ')}
{c(DIM,       '          ██╔═══╝ ██║     ██╔══██║██║╚██╗██║██╔══╝  ')}
{c(DIM,       '          ██║     ███████╗██║  ██║██║ ╚████║███████╗')}
{c(DIM,       '          ╚═╝     ╚══════╝╚═╝  ╚═╝╚═╝  ╚═══╝╚══════╝')}

  {c(BOLD, 'AI Agent Operating System')}  {c(DIM, f'v{VERSION}')}
  {c(DIM, 'The world\'s first enterprise AI agent infrastructure platform')}
""")


# ── System Utilities ──────────────────────────────────────────────────────────

def run(cmd: str, capture: bool = False, check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(
        cmd, shell=True,
        capture_output=capture,
        text=True,
        check=check,
    )


def find_root() -> Path:
    """Walk up from CWD to find the platform root (has docker-compose.yml)."""
    p = Path.cwd()
    for _ in range(10):
        if (p / COMPOSE_FILE).exists():
            return p
        if (p / "cli" / "agentplane.py").exists():
            return p
        p = p.parent
    return Path.cwd()


def check_prerequisites() -> bool:
    ok = True
    # Docker
    if not shutil.which("docker"):
        log("Docker not found — install from https://docs.docker.com/get-docker/", "error")
        ok = False
    else:
        try:
            r = run("docker --version", capture=True, check=False)
            log(f"Docker: {r.stdout.strip()}", "ok")
        except Exception:
            log("Docker found but not running", "warn")

    # Docker Compose
    try:
        r = run("docker compose version", capture=True, check=False)
        if r.returncode == 0:
            log(f"Docker Compose: {r.stdout.strip()}", "ok")
        else:
            log("Docker Compose v2 not found — upgrade Docker Desktop", "error")
            ok = False
    except Exception:
        ok = False

    # Python
    log(f"Python: {platform.python_version()}", "ok")

    return ok


def http_get(url: str, timeout: int = 3) -> Optional[dict]:
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "agentplane-cli"})
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode())
    except Exception:
        return None


def wait_for_service(name: str, url: str, max_wait: int = 120) -> bool:
    start = time.time()
    while time.time() - start < max_wait:
        if http_get(url):
            return True
        time.sleep(2)
    return False


# ── Commands ──────────────────────────────────────────────────────────────────

def cmd_install():
    banner()
    section("Platform Bootstrap — First Run Installation")

    if not check_prerequisites():
        log("Fix prerequisites then re-run: agentplane install", "error")
        sys.exit(1)

    root = find_root()
    log(f"Platform root: {root}")

    # ── Step 1: Generate .env ─────────────────────────────────────────────────
    env_file = root / ".env"
    if env_file.exists():
        log(".env already exists — skipping generation", "warn")
    else:
        example = root / ".env.example"
        if example.exists():
            shutil.copy(example, env_file)
            # Replace placeholder secrets with real random values
            content = env_file.read_text()
            content = re.sub(r"change-me-32char-minimum-secret-key", secrets.token_hex(32), content)
            content = re.sub(r"change-me-32char-secrets-passphrase", secrets.token_hex(32), content)
            content = re.sub(r"change-me-32char-brain-secret-key", secrets.token_hex(32), content)
            content = re.sub(r"change-me-federation-network-secret", secrets.token_hex(32), content)
            env_file.write_text(content)
        else:
            # Generate minimal .env from scratch
            env_file.write_text(
                f"SECRET_KEY={secrets.token_hex(32)}\n"
                f"SECRETS_PASSPHRASE={secrets.token_hex(32)}\n"
                f"BRAIN_SECRET={secrets.token_hex(32)}\n"
                f"FEDERATION_SECRET={secrets.token_hex(32)}\n"
                "POSTGRES_USER=postgres\nPOSTGRES_PASSWORD=postgres\nPOSTGRES_DB=agentdb\n"
                "DATABASE_URL=postgresql+asyncpg://postgres:postgres@postgres:5432/agentdb\n"
                "REDIS_URL=redis://redis:6379\nNATS_URL=nats://nats:4222\n"
                "OPENAI_API_KEY=\nSTRIPE_SECRET_KEY=\n"
                "CORS_ORIGINS=http://localhost:3000\n"
                "GF_SECURITY_ADMIN_USER=admin\nGF_SECURITY_ADMIN_PASSWORD=admin\n"
            )
        log(".env generated with secure random secrets", "ok")

    # ── Step 2: Pull images ───────────────────────────────────────────────────
    section("Pulling Docker Images")
    log("This may take several minutes on first run...")
    try:
        run(f"cd {root} && docker compose pull --quiet 2>/dev/null || true", check=False)
        log("Images ready", "ok")
    except Exception as e:
        log(f"Pull warning: {e}", "warn")

    # ── Step 3: Start infrastructure ──────────────────────────────────────────
    section("Starting Infrastructure Layer")
    log("Starting: postgres, redis, nats...")
    run(f"cd {root} && docker compose up -d postgres redis nats")
    log("Waiting for infrastructure to become healthy...", "dim")
    time.sleep(8)

    # Wait for postgres
    log("Waiting for PostgreSQL...", "dim")
    for i in range(30):
        r = run(f"cd {root} && docker compose exec -T postgres pg_isready -U postgres -q", capture=True, check=False)
        if r.returncode == 0:
            log("PostgreSQL ready", "ok")
            break
        time.sleep(2)
        if i == 29:
            log("PostgreSQL did not become ready in time", "error")

    # ── Step 4: Run migrations ────────────────────────────────────────────────
    section("Running Database Migrations")
    _run_migrations(root)

    # ── Step 5: Start all services ────────────────────────────────────────────
    section("Starting Platform Services")
    log("Bringing up all 35 microservices...")
    run(f"cd {root} && docker compose up -d --build")
    log("All services started", "ok")

    # ── Step 6: Wait for core services ───────────────────────────────────────
    section("Waiting for Platform Health")
    _wait_for_core_services()

    # ── Step 7: Seed demo data ────────────────────────────────────────────────
    section("Seeding Demo Data")
    _seed_demo_data()

    # ── Step 8: Show status ───────────────────────────────────────────────────
    cmd_status()

    # ── Step 9: Open dashboard ────────────────────────────────────────────────
    section("Installation Complete")
    print(f"""
  {c(GREEN, '✓')} AgentPlane is running!

  {c(BOLD, 'Control Plane Dashboard')}   {c(CYAN, DASHBOARD_URL)}
  {c(BOLD, 'API Documentation')}         {c(CYAN, API_URL + '/docs')}
  {c(BOLD, 'Control Brain')}             {c(CYAN, BRAIN_URL + '/api/v1/brain/state')}
  {c(BOLD, 'Grafana Observability')}     {c(CYAN, GRAFANA_URL)}   admin/admin
  {c(BOLD, 'Prometheus Metrics')}        {c(CYAN, 'http://localhost:9090')}

  {c(DIM, 'Run `agentplane status` to check all services')}
  {c(DIM, 'Run `agentplane logs backend` to tail API logs')}
  {c(DIM, 'Run `agentplane deploy-agent` to deploy your first agent')}
""")
    try:
        webbrowser.open(DASHBOARD_URL)
    except Exception:
        pass


def cmd_up():
    banner()
    root = find_root()

    # Check .env
    if not (root / ".env").exists():
        log(".env not found — run `agentplane install` first", "error")
        sys.exit(1)

    section("Starting AgentPlane")
    log("Starting all services...")
    run(f"cd {root} && docker compose up -d")
    log("Services started", "ok")
    time.sleep(5)
    _wait_for_core_services()
    cmd_status()


def cmd_down():
    root = find_root()
    section("Stopping AgentPlane")
    log("Stopping all services...")
    run(f"cd {root} && docker compose down")
    log("All services stopped", "ok")


def cmd_restart():
    cmd_down()
    time.sleep(2)
    cmd_up()


def cmd_status():
    root = find_root()
    section("Platform Status")

    # Docker compose status
    r = run(f"cd {root} && docker compose ps --format json", capture=True, check=False)
    running_containers: set[str] = set()
    if r.returncode == 0 and r.stdout.strip():
        for line in r.stdout.strip().split("\n"):
            try:
                svc = json.loads(line)
                name = svc.get("Service", svc.get("Name", ""))
                state = svc.get("State", svc.get("Status", ""))
                if "running" in state.lower() or "Up" in state:
                    running_containers.add(name)
            except Exception:
                pass

    # Health checks
    print(f"\n  {'SERVICE':<35} {'CONTAINER':<12} {'HTTP':<10}")
    print(f"  {'─'*35} {'─'*12} {'─'*10}")

    all_groups = [
        ("Infrastructure",  CORE_SERVICES),
        ("Control Plane",   ["backend", "control-brain", "executor", "reconciliation-service"]),
        ("Data Pipeline",   ["event-processor", "log-processor", "metrics-collector", "websocket-gateway"]),
        ("Scheduling",      ["scheduler-service", "node-manager", "global-scheduler", "region-controller", "cluster-controller"]),
        ("Agent Services",  ["agent-autoscaler", "agent-gateway", "agent-sandbox-manager",
                              "agent-simulation-engine", "agent-evolution-engine",
                              "agent-federation-network", "memory-vector-engine",
                              "agent-observability", "workflow-engine"]),
        ("Platform SaaS",   ["billing-engine", "subscription-manager", "usage-meter",
                              "secrets-manager", "marketplace-service", "marketplace-validator"]),
        ("Observability",   MONITORING_SERVICES),
        ("Frontend",        ["frontend"]),
    ]

    healthy_count = 0
    total_count = 0

    for group_name, svcs in all_groups:
        print(f"\n  {c(DIM, group_name.upper())}")
        for svc in svcs:
            total_count += 1
            container_ok = svc in running_containers
            container_sym = c(GREEN, "●") if container_ok else c(RED, "○")

            # HTTP health check
            health_url = HEALTH_MAP.get(svc)
            http_ok = False
            if health_url and container_ok:
                http_ok = http_get(health_url, timeout=2) is not None
            http_sym = c(GREEN, "OK") if http_ok else (c(DIM, "--") if not container_ok else c(YELLOW, "?"))

            if container_ok and (http_ok or health_url is None):
                healthy_count += 1

            print(f"  {svc:<35} {container_sym} {('UP' if container_ok else 'DOWN'):<10} {http_sym}")

    # Summary
    print(f"\n  {c(BOLD, 'Summary:')} {c(GREEN, str(healthy_count))} / {total_count} services healthy")

    # Brain summary
    brain_data = http_get(f"{BRAIN_URL}/api/v1/brain/state/summary", timeout=2)
    if brain_data:
        print(f"\n  {c(BOLD+CYAN, 'Control Brain State:')}")
        print(f"    Agents running:  {c(CYAN, str(brain_data.get('agents_running', '?')))}")
        print(f"    Services online: {c(CYAN, str(brain_data.get('services_healthy', '?')))}")
        print(f"    Last reconcile:  {c(DIM, str(brain_data.get('last_reconcile', '?'))[:19])}")

    print()


def cmd_logs(service: Optional[str] = None):
    root = find_root()
    if service:
        run(f"cd {root} && docker compose logs -f --tail=200 {service}")
    else:
        run(f"cd {root} && docker compose logs -f --tail=50")


def cmd_migrate():
    root = find_root()
    section("Running Database Migrations")
    _run_migrations(root)


def _run_migrations(root: Path):
    migration_dir = root / "migrations"
    if not migration_dir.exists():
        log("No migrations directory found", "warn")
        return

    sql_files = sorted(migration_dir.glob("*.sql"))
    if not sql_files:
        log("No SQL migration files found", "warn")
        return

    for f in sql_files:
        log(f"Running: {f.name}", "dim")
        r = run(
            f"cd {root} && docker compose exec -T postgres psql -U postgres -d agentdb -q < {f}",
            capture=True, check=False,
        )
        if r.returncode == 0:
            log(f"{f.name}", "ok")
        else:
            err = (r.stderr or "").strip()[:120]
            if "already exists" in err or "duplicate" in err.lower():
                log(f"{f.name} (already applied)", "ok")
            else:
                log(f"{f.name} — {err}", "warn")


def cmd_deploy_agent(name: str = "", image: str = "", org_id: str = ""):
    section("Deploy Agent Wizard")

    if not name:
        name = input(f"  Agent name [{c(DIM, 'my-agent')}]: ").strip() or "my-agent"
    if not image:
        image = input(f"  Docker image [{c(DIM, 'agent-runtime:latest')}]: ").strip() or "agent-runtime:latest"

    cpu = input(f"  CPU cores [{c(DIM, '1.0')}]: ").strip() or "1.0"
    memory = input(f"  Memory (MB) [{c(DIM, '512')}]: ").strip() or "512"
    restart = input(f"  Auto-restart on failure? [{c(DIM, 'yes')}]: ").strip().lower()
    auto_restart = restart not in ("no", "n", "false")

    payload = json.dumps({
        "name": name,
        "description": f"Agent deployed via AgentPlane CLI",
        "docker_image": image,
        "cpu_limit": float(cpu),
        "memory_limit": f"{memory}m",
        "restart_policy": "unless-stopped" if auto_restart else "no",
        "env_vars": {},
    }).encode()

    log(f"Deploying agent '{name}'...")

    try:
        req = urllib.request.Request(
            f"{API_URL}/api/v1/agents",
            data=payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            data = json.loads(resp.read().decode())
            agent_id = data.get("id", "?")
            log(f"Agent created: {c(CYAN, agent_id)}", "ok")
            log(f"Name: {name}")
            log(f"Image: {image}")
            log(f"View at: {DASHBOARD_URL}/agents/{agent_id}")
    except urllib.error.HTTPError as e:
        body = e.read().decode()
        log(f"Failed to deploy agent: HTTP {e.code} — {body[:200]}", "error")
    except Exception as ex:
        log(f"Failed to deploy agent: {ex}", "error")
        log("Is AgentPlane running? Try: agentplane up", "warn")


def cmd_reset():
    section("⚠  RESET — This will DESTROY all data!")
    confirm = input(f"  {c(RED, 'Type RESET to confirm: ')}").strip()
    if confirm != "RESET":
        log("Reset cancelled", "warn")
        return

    root = find_root()
    log("Stopping all services...")
    run(f"cd {root} && docker compose down -v --remove-orphans", check=False)
    log("All data volumes removed", "ok")
    log("Restarting fresh...")
    cmd_install()


def cmd_dashboard():
    log(f"Opening dashboard: {DASHBOARD_URL}")
    webbrowser.open(DASHBOARD_URL)
    log(f"Grafana: {GRAFANA_URL}  (admin / admin)")
    log(f"API docs: {API_URL}/docs")


def cmd_brain():
    section("Control Brain State")
    data = http_get(f"{BRAIN_URL}/api/v1/brain/state", timeout=5)
    if not data:
        log("Control Brain unreachable. Is it running?", "error")
        log(f"  Check: {BRAIN_URL}/health", "dim")
        return

    summary = data.get("summary", {})
    print(f"\n  {c(BOLD, 'Global State')}")
    print(f"    Agents total:    {c(CYAN, str(summary.get('agents_total', '?')))}")
    print(f"    Agents running:  {c(GREEN, str(summary.get('agents_running', '?')))}")
    print(f"    Agents error:    {c(RED, str(summary.get('agents_error', '?')))}")
    print(f"    Services:        {summary.get('services_healthy','?')}/{summary.get('services_total','?')} healthy")
    print(f"    Nodes:           {c(CYAN, str(summary.get('nodes_total', '?')))}")
    print(f"    Last reconcile:  {c(DIM, str(summary.get('last_reconcile','never'))[:19])}")
    print(f"    Uptime:          {c(DIM, str(int(summary.get('uptime_seconds',0)))+'s')}")

    agents = data.get("agents", [])
    if agents:
        print(f"\n  {c(BOLD, 'Agent States')} (top 10)")
        print(f"  {'AGENT ID':<38} {'STATUS':<12} {'NODE'}")
        print(f"  {'─'*38} {'─'*12} {'─'*20}")
        for ag in agents[:10]:
            st = ag.get("status", "unknown")
            col = GREEN if st == "running" else (RED if st == "error" else DIM)
            print(f"  {ag.get('agent_id','?'):<38} {c(col, st.upper()):<20} {ag.get('node_id','?')[:20]}")


def cmd_services():
    section("Registered Microservices")
    data = http_get(f"{API_URL}/api/v1/brain/services", timeout=5)
    if not data:
        log("Could not reach API. Is AgentPlane running?", "error")
        return

    svcs = data if isinstance(data, list) else data.get("services", [])
    print(f"\n  {'SERVICE':<35} {'STATUS':<12} {'PORT':<8} {'LAST SEEN'}")
    print(f"  {'─'*35} {'─'*12} {'─'*8} {'─'*20}")
    for svc in svcs:
        st = svc.get("status", "unknown")
        col = GREEN if st == "healthy" else (YELLOW if st == "degraded" else RED)
        url = svc.get("service_url", "")
        port = url.split(":")[-1] if url else "?"
        seen = svc.get("last_seen", "?")[:19] if svc.get("last_seen") else "?"
        caps = ", ".join(svc.get("capabilities", []))[:30]
        print(f"  {svc.get('service_name','?'):<35} {c(col, st.upper()):<20} {port:<8} {c(DIM, seen)}")


def cmd_version():
    banner()
    print(f"  Version:     {c(BOLD+CYAN, VERSION)}")
    print(f"  Platform:    AI Agent Operating System")
    print(f"  Services:    35 microservices")
    print(f"  Language:    Python (backend), Go (microservices), TypeScript (frontend)")
    print(f"  Database:    PostgreSQL 16 + pgvector + Redis 7 + NATS JetStream")
    print()


# ── Internal Helpers ──────────────────────────────────────────────────────────

def _wait_for_core_services():
    log("Waiting for Backend API...")
    if wait_for_service("backend", f"{API_URL}/health", max_wait=120):
        log("Backend API ready", "ok")
    else:
        log("Backend API did not respond in 120s — check: agentplane logs backend", "warn")

    log("Waiting for Control Brain...")
    if wait_for_service("control-brain", f"{BRAIN_URL}/health", max_wait=60):
        log("Control Brain ready", "ok")
    else:
        log("Control Brain not responding yet — it may still be starting", "warn")


def _seed_demo_data():
    """Seed a demo agent if the platform is fresh."""
    try:
        req = urllib.request.Request(f"{API_URL}/api/v1/agents?limit=1")
        with urllib.request.urlopen(req, timeout=5) as resp:
            agents = json.loads(resp.read().decode())
            if agents:
                log("Demo data already exists — skipping seed", "dim")
                return
    except Exception:
        return

    try:
        payload = json.dumps({
            "name": "demo-research-agent",
            "description": "Demo agent — deployed by AgentPlane installer",
            "docker_image": "agent-runtime:latest",
            "cpu_limit": 0.5,
            "memory_limit": "256m",
            "restart_policy": "unless-stopped",
            "env_vars": {"AGENT_ROLE": "research"},
        }).encode()
        req = urllib.request.Request(
            f"{API_URL}/api/v1/agents",
            data=payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode())
            log(f"Demo agent created: {data.get('id','?')}", "ok")
    except Exception as e:
        log(f"Demo seed skipped: {e}", "dim")


# ── Main ──────────────────────────────────────────────────────────────────────

def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__)
        return

    cmd = args[0]
    rest = args[1:]

    dispatch = {
        "install":      cmd_install,
        "up":           cmd_up,
        "start":        cmd_up,
        "down":         cmd_down,
        "stop":         cmd_down,
        "restart":      cmd_restart,
        "status":       cmd_status,
        "logs":         lambda: cmd_logs(rest[0] if rest else None),
        "migrate":      cmd_migrate,
        "deploy-agent": lambda: cmd_deploy_agent(*rest[:3]) if rest else cmd_deploy_agent(),
        "reset":        cmd_reset,
        "dashboard":    cmd_dashboard,
        "brain":        cmd_brain,
        "services":     cmd_services,
        "version":      cmd_version,
        "--version":    cmd_version,
        "-v":           cmd_version,
    }

    if cmd not in dispatch:
        log(f"Unknown command: {cmd}", "error")
        print(__doc__)
        sys.exit(1)

    try:
        dispatch[cmd]()
    except KeyboardInterrupt:
        print(f"\n  {c(DIM, 'Interrupted')}")
        sys.exit(0)


if __name__ == "__main__":
    main()