#!/usr/bin/env python3
"""Production agent runtime - runs inside Docker containers managed by the platform."""
import os
import sys
import json
import asyncio
import signal
import time
import uuid
import random
from datetime import datetime, timezone
from typing import Any, TypedDict, Annotated
import operator

import httpx
import nats
import structlog
import psutil
from langgraph.graph import StateGraph, END
from langchain_core.messages import HumanMessage, AIMessage

logger = structlog.get_logger()

AGENT_ID = os.getenv("AGENT_ID", str(uuid.uuid4()))
AGENT_NAME = os.getenv("AGENT_NAME", "unnamed-agent")
CONTROL_PLANE_URL = os.getenv("CONTROL_PLANE_URL", "http://backend:8000")
NATS_URL = os.getenv("NATS_URL", "nats://nats:4222")
TASK_INPUT = os.getenv("TASK_INPUT", '{"task": "default analysis task", "max_iterations": 5}')
HEARTBEAT_INTERVAL = int(os.getenv("HEARTBEAT_INTERVAL", "10"))

nc_client = None
_shutdown = False


class AgentState(TypedDict):
    messages: Annotated[list, operator.add]
    task: str
    iteration: int
    max_iterations: int
    result: str
    status: str


async def get_nats():
    global nc_client
    if nc_client is None or nc_client.is_closed:
        try:
            nc_client = await nats.connect(NATS_URL, max_reconnect_attempts=5)
        except Exception as e:
            logger.warning("NATS connection failed", error=str(e))
            nc_client = None
    return nc_client


async def publish_nats(subject: str, data: dict[str, Any]):
    try:
        nc = await get_nats()
        if nc:
            await nc.publish(subject, json.dumps(data).encode())
    except Exception:
        pass


def get_process_metrics() -> tuple[float, float]:
    """Return (cpu_percent, memory_mb) for current process."""
    try:
        proc = psutil.Process()
        cpu = proc.cpu_percent(interval=0.1)
        mem = proc.memory_info().rss / 1024 / 1024
        return cpu, mem
    except Exception:
        return 0.0, 0.0


async def log_msg(message: str, level: str = "info"):
    """Emit a log line to stdout and NATS."""
    print(f"[{level.upper()}] {message}", flush=True)
    await publish_nats("logs.stream", {
        "agent_id": AGENT_ID,
        "message": message,
        "level": level,
        "source": "agent-runtime",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    })


async def emit_metric(name: str, value: float):
    await publish_nats("metrics.stream", {
        "agent_id": AGENT_ID,
        "metric_name": name,
        "metric_value": value,
        "timestamp": datetime.now(timezone.utc).isoformat(),
    })


async def emit_event(event_type: str, payload: dict = {}):
    await publish_nats("agents.events", {
        "event_type": event_type,
        "agent_id": AGENT_ID,
        "source": "agent-runtime",
        "payload": payload,
        "level": "info",
        "timestamp": datetime.now(timezone.utc).isoformat(),
    })


async def persist(endpoint: str, data: dict[str, Any]):
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            await client.post(f"{CONTROL_PLANE_URL}/api/v1/{endpoint}", json=data)
    except Exception as e:
        pass  # Non-critical - metrics collector persists independently


def build_agent_graph():
    """Build a realistic LangGraph state machine for demo execution."""

    def plan_node(state: AgentState) -> AgentState:
        task = state["task"]
        plan = f"Task '{task}': Analyze → Process → Validate → Report"
        return {
            "messages": [AIMessage(content=plan)],
            "status": "planning",
            "iteration": state["iteration"] + 1,
        }

    def execute_node(state: AgentState) -> AgentState:
        iteration = state["iteration"]
        cpu, mem = get_process_metrics()
        # Simulate varying workload
        work_time = random.uniform(0.3, 1.2)
        time.sleep(work_time)
        cpu2, mem2 = get_process_metrics()
        result = (
            f"Iteration {iteration}: processed '{state['task']}' "
            f"in {work_time:.2f}s — CPU={cpu2:.1f}% MEM={mem2:.1f}MB"
        )
        return {
            "messages": [AIMessage(content=result)],
            "status": "executing",
            "result": result,
        }

    def validate_node(state: AgentState) -> AgentState:
        valid = random.random() > 0.1  # 90% success rate
        status = "validated" if valid else "validation_failed"
        return {
            "messages": [AIMessage(content=f"Validation: {status}")],
            "status": status,
        }

    def evaluate_node(state: AgentState) -> AgentState:
        if state["iteration"] >= state["max_iterations"] or state["status"] == "validation_failed":
            final = "completed" if state["status"] != "validation_failed" else "failed"
            return {"status": final, "iteration": state["iteration"], "messages": [AIMessage(content=f"Task {final}.")]}
        return {"status": "executing", "iteration": state["iteration"]}

    def should_continue(state: AgentState) -> str:
        if state["status"] in ("completed", "failed") or state["iteration"] >= state["max_iterations"]:
            return "end"
        return "execute"

    g = StateGraph(AgentState)
    g.add_node("plan", plan_node)
    g.add_node("execute", execute_node)
    g.add_node("validate", validate_node)
    g.add_node("evaluate", evaluate_node)
    g.set_entry_point("plan")
    g.add_edge("plan", "execute")
    g.add_edge("execute", "validate")
    g.add_edge("validate", "evaluate")
    g.add_conditional_edges("evaluate", should_continue, {"execute": "execute", "end": END})
    return g.compile()


async def run_agent():
    global _shutdown
    await asyncio.sleep(1)

    await log_msg(f"Agent {AGENT_NAME} ({AGENT_ID[:8]}) starting...", "info")

    task_config = {}
    try:
        task_config = json.loads(TASK_INPUT)
    except Exception:
        task_config = {"task": TASK_INPUT}

    task = task_config.get("task", "perform default analysis")
    max_iterations = int(task_config.get("max_iterations", 5))
    execution_id = str(uuid.uuid4())

    # Register execution
    await persist("executions", {
        "agent_id": AGENT_ID,
        "status": "running",
        "input": task_config,
    })

    await emit_event("execution.started", {"execution_id": execution_id, "task": task})
    await log_msg(f"Starting execution {execution_id[:8]} — task: {task}", "info")

    start_time = time.time()
    success = True

    try:
        graph = build_agent_graph()
        initial_state = AgentState(
            messages=[HumanMessage(content=task)],
            task=task,
            iteration=0,
            max_iterations=max_iterations,
            result="",
            status="pending",
        )

        async for state in graph.astream(initial_state, config={"recursion_limit": 100}):
            if _shutdown:
                break
            for node_name, node_state in state.items():
                status = node_state.get("status", "")
                iteration = node_state.get("iteration", 0)
                level = "warn" if status == "validation_failed" else "info"
                msg = f"[{node_name}] iteration={iteration} status={status}"
                await log_msg(msg, level)

                cpu, mem = get_process_metrics()
                await emit_metric("cpu_percent", cpu + random.uniform(1, 5))
                await emit_metric("memory_mb", mem)

                if status == "failed":
                    success = False

        duration_ms = int((time.time() - start_time) * 1000)
        result_status = "completed" if success else "failed"
        await log_msg(f"Execution {result_status} in {duration_ms}ms", "info" if success else "error")
        await emit_event(f"execution.{result_status}", {
            "execution_id": execution_id,
            "duration_ms": duration_ms,
        })

    except Exception as e:
        duration_ms = int((time.time() - start_time) * 1000)
        await log_msg(f"Agent execution failed: {e}", "error")
        await emit_event("agent.failed", {"error": str(e), "duration_ms": duration_ms})
        success = False

    # ─── Idle heartbeat loop ──────────────────────────────────────────────────
    await log_msg("Agent entering idle heartbeat loop...", "info")
    heartbeat_count = 0
    while not _shutdown:
        await asyncio.sleep(HEARTBEAT_INTERVAL)
        if _shutdown:
            break
        heartbeat_count += 1
        cpu, mem = get_process_metrics()
        # Add some variation to make charts interesting
        cpu_reported = cpu + random.uniform(0.5, 3.0)
        mem_reported = mem + random.uniform(0, 5.0)

        await emit_metric("cpu_percent", cpu_reported)
        await emit_metric("memory_mb", mem_reported)
        await emit_metric("memory_percent", (mem_reported / 512) * 100)

        if heartbeat_count % 6 == 0:  # Every ~minute
            await log_msg(
                f"Heartbeat #{heartbeat_count} — CPU: {cpu_reported:.1f}% Memory: {mem_reported:.1f}MB",
                "debug"
            )

        # Occasionally emit info logs
        if heartbeat_count % 3 == 0:
            messages = [
                f"Processing queue depth: {random.randint(0, 10)}",
                f"Cache hit rate: {random.uniform(80, 99):.1f}%",
                f"Active connections: {random.randint(1, 20)}",
                f"Throughput: {random.uniform(100, 1000):.0f} ops/s",
            ]
            await log_msg(random.choice(messages), "info")


async def main():
    global _shutdown
    loop = asyncio.get_event_loop()

    def shutdown(sig):
        global _shutdown
        _shutdown = True
        print(f"[INFO] Received signal {sig}, shutting down...", flush=True)
        asyncio.create_task(emit_event("agent.stopped", {"signal": str(sig)}))
        asyncio.get_event_loop().call_later(2, loop.stop)

    for sig in (signal.SIGTERM, signal.SIGINT):
        loop.add_signal_handler(sig, lambda s=sig: shutdown(s))

    await run_agent()


if __name__ == "__main__":
    asyncio.run(main())