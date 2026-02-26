import uuid
from datetime import datetime
from typing import Any, Optional
from pydantic import BaseModel, EmailStr, Field, ConfigDict


class UserCreate(BaseModel):
    username: str
    email: EmailStr
    password: str


class UserResponse(BaseModel):
    id: uuid.UUID
    username: str
    email: str
    is_active: bool
    created_at: datetime
    model_config = {"from_attributes": True}


class AgentCreate(BaseModel):
    name: str
    description: Optional[str] = None
    agent_type: str = "langgraph"
    config: dict[str, Any] = {}
    image: str = "agent-runtime:latest"
    cpu_limit: float = 1.0
    memory_limit: str = "512m"
    env_vars: dict[str, str] = {}
    labels: dict[str, str] = {}
    auto_restart: bool = False


class AgentUpdate(BaseModel):
    name: Optional[str] = None
    description: Optional[str] = None
    config: Optional[dict[str, Any]] = None
    cpu_limit: Optional[float] = None
    memory_limit: Optional[str] = None
    env_vars: Optional[dict[str, str]] = None
    auto_restart: Optional[bool] = None


class AgentResponse(BaseModel):
    id: uuid.UUID
    name: str
    description: Optional[str]
    agent_type: str
    config: dict[str, Any]
    image: str
    status: str
    container_id: Optional[str]
    container_name: Optional[str]
    cpu_limit: float
    memory_limit: str
    env_vars: dict[str, Any]
    labels: dict[str, Any]
    auto_restart: bool
    restart_count: int
    last_error: Optional[str]
    created_at: datetime
    updated_at: datetime
    model_config = {"from_attributes": True}


class AgentAction(BaseModel):
    action: str


class ExecutionCreate(BaseModel):
    agent_id: uuid.UUID
    status: str = "pending"
    input: dict[str, Any] = {}


class ExecutionResponse(BaseModel):
    id: uuid.UUID
    agent_id: uuid.UUID
    status: str
    input: dict[str, Any]
    output: dict[str, Any]
    error: Optional[str]
    started_at: Optional[datetime]
    completed_at: Optional[datetime]
    duration_ms: Optional[int]
    exit_code: Optional[int]
    created_at: datetime
    model_config = {"from_attributes": True}


class LogCreate(BaseModel):
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID] = None
    level: str = "info"
    message: str
    source: Optional[str] = None
    metadata: dict[str, Any] = {}


class LogResponse(BaseModel):
    id: uuid.UUID
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID]
    level: str
    message: str
    source: Optional[str]
    # ORM attr is 'meta'; validation_alias lets Pydantic read it while the
    # JSON key stays 'metadata' so the frontend contract is unchanged.
    metadata: dict[str, Any] = Field(default={}, validation_alias="meta")
    timestamp: datetime
    model_config = ConfigDict(from_attributes=True, populate_by_name=True)


class EventCreate(BaseModel):
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID] = None
    event_type: str
    payload: dict[str, Any] = {}
    source: Optional[str] = None
    level: str = "info"


class EventResponse(BaseModel):
    id: uuid.UUID
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID]
    event_type: str
    payload: dict[str, Any]
    source: Optional[str]
    level: str
    created_at: datetime
    model_config = {"from_attributes": True}


class MetricCreate(BaseModel):
    agent_id: uuid.UUID
    metric_name: str
    metric_value: float
    labels: dict[str, Any] = {}


class MetricResponse(BaseModel):
    id: uuid.UUID
    agent_id: uuid.UUID
    metric_name: str
    metric_value: float
    labels: dict[str, Any]
    timestamp: datetime
    model_config = {"from_attributes": True}


class AlertRuleCreate(BaseModel):
    name: str
    description: Optional[str] = None
    metric_name: str
    condition: str  # gt, lt, gte, lte, eq
    threshold: float
    severity: str = "warning"
    enabled: bool = True
    agent_id: Optional[uuid.UUID] = None


class AlertRuleResponse(BaseModel):
    id: str
    name: str
    description: Optional[str] = None
    metric_name: str
    condition: str
    threshold: float
    severity: str
    enabled: bool
    agent_id: Optional[str] = None
    created_at: datetime
    updated_at: datetime
    model_config = {"from_attributes": True}

    @classmethod
    def from_orm(cls, obj):
        return cls(
            id=str(obj.id),
            name=obj.name,
            description=obj.description,
            metric_name=obj.metric_name,
            condition=obj.condition,
            threshold=obj.threshold,
            severity=obj.severity,
            enabled=obj.enabled,
            agent_id=str(obj.agent_id) if obj.agent_id else None,
            created_at=obj.created_at,
            updated_at=obj.updated_at,
        )


class AlertResponse(BaseModel):
    id: uuid.UUID
    rule_id: Optional[uuid.UUID]
    agent_id: Optional[uuid.UUID]
    metric_name: str
    metric_value: float
    threshold: float
    severity: str
    message: str
    resolved: bool
    resolved_at: Optional[datetime]
    created_at: datetime
    model_config = {"from_attributes": True}


class ContainerResponse(BaseModel):
    id: str
    agent_id: str
    container_id: str
    container_name: Optional[str] = None
    image: Optional[str] = None
    status: str
    started_at: Optional[datetime] = None
    stopped_at: Optional[datetime] = None
    exit_code: Optional[int] = None
    created_at: datetime
    model_config = {"from_attributes": True}

    @classmethod
    def from_orm(cls, obj):
        return cls(
            id=str(obj.id),
            agent_id=str(obj.agent_id),
            container_id=obj.container_id,
            container_name=obj.container_name,
            image=obj.image,
            status=obj.status,
            started_at=obj.started_at,
            stopped_at=obj.stopped_at,
            exit_code=obj.exit_code,
            created_at=obj.created_at,
        )