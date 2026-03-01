from __future__ import annotations

import json
from pathlib import Path
from typing import Optional

import typer

from sdk.python.agentcloud.client import AgentCloudClient

app = typer.Typer(help="Platform CLI")


def client(base_url: str, api_key: Optional[str]) -> AgentCloudClient:
    return AgentCloudClient(base_url=base_url, api_key=api_key)


@app.command()
def init(path: str = typer.Option(".")):
    root = Path(path).resolve()
    (root / ".platform").mkdir(parents=True, exist_ok=True)
    config_path = root / ".platform" / "config.json"
    if not config_path.exists():
        config_path.write_text(json.dumps({"api_url": "http://localhost:8000"}, indent=2), encoding="utf-8")
    typer.echo(str(config_path))


@app.command()
def deploy(
    name: str = typer.Option(...),
    image: str = typer.Option(...),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    result = client(base_url, api_key).deploy_agent(name=name, image=image)
    typer.echo(json.dumps(result, default=str))


@app.command()
def logs(
    agent_id: str = typer.Option(...),
    limit: int = typer.Option(100),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    result = client(base_url, api_key).get_logs(agent_id=agent_id, limit=limit)
    typer.echo(json.dumps(result, default=str))


@app.command()
def scale(
    agent_id: str = typer.Option(...),
    replicas: int = typer.Option(...),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    result = client(base_url, api_key).scale_agent(agent_id=agent_id, replicas=replicas)
    typer.echo(json.dumps(result, default=str))


@app.command()
def rollback(
    agent_id: str = typer.Option(...),
    version: str = typer.Option(...),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    result = client(base_url, api_key).rollback_agent(agent_id=agent_id, version=version)
    typer.echo(json.dumps(result, default=str))


@app.command()
def workflows(
    workflow_id: str = typer.Option(...),
    payload: str = typer.Option("{}"),
    base_url: str = typer.Option("http://localhost:8000"),
    api_key: Optional[str] = typer.Option(None),
):
    result = client(base_url, api_key).run_workflow(workflow_id=workflow_id, input_payload=json.loads(payload))
    typer.echo(json.dumps(result, default=str))


if __name__ == "__main__":
    app()
