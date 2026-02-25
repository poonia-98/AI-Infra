import uuid
from datetime import datetime
from typing import Any, Optional
from pydantic import BaseModel, EmailStr, Field, ConfigDict


# ─── User ─────────────────────────────────────────────────────────────────────

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
    model_config = ConfigDict(from_attributes=True)


# ─── Agent ────────────────────────────────────────────────────────────────────

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
    created_at: datetime
    updated_at: datetime
    model_config = ConfigDict(from_attributes=True)


class AgentAction(BaseModel):
    action: str
    params: dict[str, Any] = {}


# ─── Execution ────────────────────────────────────────────────────────────────

class ExecutionCreate(BaseModel):
    agent_id: uuid.UUID
    status: str = "running"
    input: dict[str, Any] = {}


class ExecutionUpdate(BaseModel):
    status: str
    output: dict[str, Any] = {}
    error: Optional[str] = None
    duration_ms: Optional[int] = None
    exit_code: Optional[int] = None


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
    model_config = ConfigDict(from_attributes=True)


# ─── Log ──────────────────────────────────────────────────────────────────────

class LogCreate(BaseModel):
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID] = None
    level: str = "info"
    message: str
    source: Optional[str] = None
    metadata: dict[str, Any] = {}   # received from clients as 'metadata'


class LogResponse(BaseModel):
    id: uuid.UUID
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID]
    level: str
    message: str
    source: Optional[str]
    # ORM attr is 'meta' (to avoid SQLAlchemy conflict) but we expose it as 'metadata' in JSON.
    # validation_alias reads from ORM attr 'meta'; field name 'metadata' is used for serialization.
    metadata: dict[str, Any] = Field(default={}, validation_alias="log_metadata")
    timestamp: datetime
    model_config = ConfigDict(from_attributes=True, populate_by_name=True)


# ─── Event ────────────────────────────────────────────────────────────────────

class EventCreate(BaseModel):
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID] = None
    event_type: str
    payload: dict[str, Any] = {}
    source: Optional[str] = None


class EventResponse(BaseModel):
    id: uuid.UUID
    agent_id: uuid.UUID
    execution_id: Optional[uuid.UUID]
    event_type: str
    payload: dict[str, Any]
    source: Optional[str]
    created_at: datetime
    model_config = ConfigDict(from_attributes=True)


# ─── Metric ───────────────────────────────────────────────────────────────────

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
    model_config = ConfigDict(from_attributes=True)


# ─── System ───────────────────────────────────────────────────────────────────

class SystemHealth(BaseModel):
    status: str
    total_agents: int
    running_agents: int
    failed_agents: int
    total_executions: int
    active_executions: int
    uptime_seconds: float
class AlertResponse(BaseModel):
    id: int
    name: str
    message: str
    status: str
    created_at: datetime
    class Config:
        from_attributes = True


class AlertRuleCreate(BaseModel):
    name: str
    condition: str
    class Config:
        from_attributes = True

class AlertRuleResponse(BaseModel):
    id: int
    name: str
    condition: str
    created_at: datetime
    class Config:
        from_attributes = True

class ContainerResponse(BaseModel):
    id: str
    name: str
    status: str
    class Config:
        from_attributes = True
