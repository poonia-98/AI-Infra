from typing import Any, Optional
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncSession
from database import get_db
from services.workflow_engine import workflow_engine_service
from security.auth import AuthenticatedUser, get_current_user


router = APIRouter(prefix="/api/v1/workflows", tags=["workflows"])


class WorkflowCreate(BaseModel):
    name: str
    description: Optional[str] = None
    dag: dict[str, Any] = Field(default_factory=lambda: {"nodes": [], "edges": []})
    trigger_type: str = "manual"
    trigger_config: dict[str, Any] = Field(default_factory=dict)
    created_by: Optional[str] = None


class WorkflowExecute(BaseModel):
    input: dict[str, Any] = Field(default_factory=dict)
    triggered_by: Optional[str] = None


@router.post("", status_code=201)
async def create_workflow(
    payload: WorkflowCreate,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    workflow_id = await workflow_engine_service.create_workflow(
        db=db,
        name=payload.name,
        dag=payload.dag,
        organisation_id=user.organisation_id,
        description=payload.description,
        trigger_type=payload.trigger_type,
        trigger_config=payload.trigger_config,
        created_by=payload.created_by or user.user_id,
    )
    return {"id": workflow_id}


@router.get("")
async def list_workflows(
    status: Optional[str] = None,
    limit: int = Query(100, ge=1, le=500),
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    return await workflow_engine_service.list_workflows(
        db=db,
        organisation_id=user.organisation_id,
        status=status,
        limit=limit,
    )


@router.get("/{workflow_id}")
async def get_workflow(workflow_id: str, db: AsyncSession = Depends(get_db)):
    item = await workflow_engine_service.get_workflow(db, workflow_id)
    if not item:
        raise HTTPException(status_code=404, detail="Workflow not found")
    return item


@router.patch("/{workflow_id}")
async def update_workflow(workflow_id: str, payload: dict[str, Any], db: AsyncSession = Depends(get_db)):
    ok = await workflow_engine_service.update_workflow(db, workflow_id, payload)
    if not ok:
        raise HTTPException(status_code=404, detail="Workflow not found or empty patch")
    return {"status": "ok"}


@router.post("/{workflow_id}/execute", status_code=202)
async def execute_workflow(
    workflow_id: str,
    payload: WorkflowExecute,
    db: AsyncSession = Depends(get_db),
    user: AuthenticatedUser = Depends(get_current_user),
):
    workflow = await workflow_engine_service.get_workflow(db, workflow_id)
    if not workflow:
        raise HTTPException(status_code=404, detail="Workflow not found")
    if workflow.get("organisation_id") and workflow.get("organisation_id") != user.organisation_id:
        raise HTTPException(status_code=403, detail="Forbidden")
    execution_id = await workflow_engine_service.trigger_execution(
        db=db,
        workflow_id=workflow_id,
        input_payload=payload.input,
        triggered_by=payload.triggered_by or user.user_id,
        organisation_id=user.organisation_id,
    )
    return {"execution_id": execution_id, "status": "pending"}


@router.get("/executions/{execution_id}")
async def get_execution(execution_id: str, db: AsyncSession = Depends(get_db)):
    execution = await workflow_engine_service.get_execution(db, execution_id)
    if not execution:
        raise HTTPException(status_code=404, detail="Execution not found")
    return execution


@router.get("/executions/{execution_id}/nodes")
async def list_execution_nodes(execution_id: str, db: AsyncSession = Depends(get_db)):
    return await workflow_engine_service.list_node_executions(db, execution_id)
