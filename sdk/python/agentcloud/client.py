from __future__ import annotations

from typing import Any, Optional
import httpx


class AgentCloudClient:
    def __init__(self, base_url: str, api_key: Optional[str] = None, timeout: float = 30.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.client = httpx.Client(timeout=timeout)

    def _headers(self) -> dict[str, str]:
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["X-API-Key"] = self.api_key
        return headers

    def _get(self, path: str, params: Optional[dict[str, Any]] = None) -> Any:
        response = self.client.get(f"{self.base_url}{path}", params=params, headers=self._headers())
        response.raise_for_status()
        return response.json()

    def _post(self, path: str, payload: Optional[dict[str, Any]] = None) -> Any:
        response = self.client.post(f"{self.base_url}{path}", json=payload or {}, headers=self._headers())
        response.raise_for_status()
        if response.content:
            return response.json()
        return {}

    def _patch(self, path: str, payload: Optional[dict[str, Any]] = None) -> Any:
        response = self.client.patch(f"{self.base_url}{path}", json=payload or {}, headers=self._headers())
        response.raise_for_status()
        if response.content:
            return response.json()
        return {}

    def deploy_agent(
        self,
        name: str,
        image: str,
        agent_type: str = "langgraph",
        config: Optional[dict[str, Any]] = None,
        env_vars: Optional[dict[str, str]] = None,
        labels: Optional[dict[str, str]] = None,
        cpu_limit: float = 1.0,
        memory_limit: str = "512m",
        auto_start: bool = True,
    ) -> dict[str, Any]:
        created = self._post(
            "/api/v1/agents",
            {
                "name": name,
                "image": image,
                "agent_type": agent_type,
                "config": config or {},
                "env_vars": env_vars or {},
                "labels": labels or {},
                "cpu_limit": cpu_limit,
                "memory_limit": memory_limit,
            },
        )
        if auto_start and created.get("id"):
            self._post(f"/api/v1/agents/{created['id']}/start")
        return created

    def scale_agent(self, agent_id: str, replicas: int) -> dict[str, Any]:
        return self._post(f"/api/v1/agents/{agent_id}/scale", {"desired_replicas": replicas})

    def rollback_agent(self, agent_id: str, version: str) -> dict[str, Any]:
        return self._post(f"/api/v1/agents/{agent_id}/rollback", {"version": version})

    def start_agent(self, agent_id: str) -> dict[str, Any]:
        return self._post(f"/api/v1/agents/{agent_id}/start")

    def stop_agent(self, agent_id: str) -> dict[str, Any]:
        return self._post(f"/api/v1/agents/{agent_id}/stop")

    def list_agents(self, limit: int = 100) -> list[dict[str, Any]]:
        return self._get("/api/v1/agents", params={"limit": limit})

    def get_logs(self, agent_id: str, limit: int = 200) -> list[dict[str, Any]]:
        return self._get(f"/api/v1/agents/{agent_id}/logs", params={"limit": limit})

    def run_workflow(self, workflow_id: str, input_payload: Optional[dict[str, Any]] = None) -> dict[str, Any]:
        return self._post(f"/api/v1/workflows/{workflow_id}/execute", {"input": input_payload or {}})

    def get_workflow_execution(self, execution_id: str) -> dict[str, Any]:
        return self._get(f"/api/v1/workflows/executions/{execution_id}")

    def inspect_memory(self, agent_id: str, limit: int = 50, memory_type: Optional[str] = None) -> list[dict[str, Any]]:
        payload: dict[str, Any] = {"agent_id": agent_id, "limit": limit}
        if memory_type:
            payload["memory_type"] = memory_type
        return self._post("/api/v1/memory/recall", payload)

    def trigger_execution(
        self,
        agent_id: str,
        input_payload: Optional[dict[str, Any]] = None,
        priority: int = 5,
        max_attempts: int = 3,
        dedup_key: Optional[str] = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "agent_id": agent_id,
            "input": input_payload or {},
            "priority": priority,
            "max_attempts": max_attempts,
        }
        if dedup_key:
            payload["dedup_key"] = dedup_key
        return self._post("/queue/enqueue", payload)

    def close(self) -> None:
        self.client.close()
