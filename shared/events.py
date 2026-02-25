"""Shared event schema definitions — used by FastAPI backend and agent runtime."""
from __future__ import annotations
from enum import Enum
from typing import Any, Optional
from pydantic import BaseModel, Field
from datetime import datetime, timezone
import uuid


class EventType(str, Enum):
    AGENT_CREATED   = "agent.created"
    AGENT_STARTED   = "agent.started"
    AGENT_STOPPED   = "agent.stopped"
    AGENT_FAILED    = "agent.failed"
    AGENT_COMPLETED = "agent.completed"
    AGENT_DELETED   = "agent.deleted"
    AGENT_HEARTBEAT = "agent.heartbeat"


class LogLevel(str, Enum):
    DEBUG   = "debug"
    INFO    = "info"
    WARN    = "warn"
    WARNING = "warning"
    ERROR   = "error"
    FATAL   = "fatal"


class NATSSubject(str, Enum):
    AGENTS_EVENTS  = "agents.events"
    LOGS_STREAM    = "logs.stream"
    EVENTS_STREAM  = "events.stream"
    METRICS_STREAM = "metrics.stream"


class BaseEvent(BaseModel):
    event_type:   str
    agent_id:     str
    execution_id: Optional[str] = None
    timestamp:    str = Field(default_factory=lambda: datetime.now(timezone.utc).isoformat())
    source:       str = "unknown"
    payload:      dict[str, Any] = Field(default_factory=dict)

    @classmethod
    def create(cls, event_type: EventType | str, agent_id: str, source: str, **payload: Any) -> "BaseEvent":
        return cls(
            event_type=str(event_type),
            agent_id=agent_id,
            source=source,
            payload=payload,
        )


class LogEvent(BaseModel):
    agent_id:     str
    execution_id: Optional[str] = None
    message:      str
    level:        str = "info"
    source:       Optional[str] = None
    timestamp:    str = Field(default_factory=lambda: datetime.now(timezone.utc).isoformat())
    metadata:     dict[str, Any] = Field(default_factory=dict)


class MetricEvent(BaseModel):
    agent_id:     str
    metric_name:  str
    metric_value: float
    labels:       dict[str, Any] = Field(default_factory=dict)
    timestamp:    str = Field(default_factory=lambda: datetime.now(timezone.utc).isoformat())


class AgentStartedPayload(BaseModel):
    container_id:   str
    container_name: str
    image:          str


class AgentStoppedPayload(BaseModel):
    container_id: str
    exit_code:    Optional[int] = None


class AgentFailedPayload(BaseModel):
    error:        str
    container_id: Optional[str] = None
    exit_code:    Optional[int] = None


class AgentHeartbeatPayload(BaseModel):
    cpu_percent:    float
    memory_mb:      float
    memory_percent: float
    uptime_seconds: float