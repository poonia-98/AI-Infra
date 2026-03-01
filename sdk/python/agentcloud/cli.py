from __future__ import annotations

import json
from typing import Optional

import typer

from .client import AgentCloudClient

app = typer.Typer(help="Agent Cloud CLI")


def _client(base_url: str, api_key: Optional[str]) -> AgentCloudClient:
    return AgentCloudClient(base_url=base_url, api_key=api_key)


@app.command()
def deploy(
    name: str = typer.Option(...),
    image: str = typer.Option(...),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
    auto_start: bool = typer.Option(True),
):
    client = _client(base_url, api_key)
    result = client.deploy_agent(name=name, image=image, auto_start=auto_start)
    typer.echo(json.dumps(result, default=str))


@app.command()
def scale(
    agent_id: str = typer.Option(...),
    replicas: int = typer.Option(...),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    client = _client(base_url, api_key)
    result = client.scale_agent(agent_id, replicas)
    typer.echo(json.dumps(result, default=str))


@app.command()
def workflows(
    workflow_id: str = typer.Option(...),
    payload: str = typer.Option("{}"),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    client = _client(base_url, api_key)
    result = client.run_workflow(workflow_id, json.loads(payload))
    typer.echo(json.dumps(result, default=str))


@app.command()
def logs(
    agent_id: str = typer.Option(...),
    limit: int = typer.Option(100),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    client = _client(base_url, api_key)
    result = client.get_logs(agent_id, limit)
    typer.echo(json.dumps(result, default=str))


@app.command()
def memory(
    agent_id: str = typer.Option(...),
    limit: int = typer.Option(50),
    memory_type: Optional[str] = typer.Option(None),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    client = _client(base_url, api_key)
    result = client.inspect_memory(agent_id, limit=limit, memory_type=memory_type)
    typer.echo(json.dumps(result, default=str))


if __name__ == "__main__":
    app()
