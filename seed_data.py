#!/usr/bin/env python3
"""
Seed script to populate agents with test data.
"""
import asyncio
import uuid
import sys
import os
from datetime import datetime, timedelta
from sqlalchemy.ext.asyncio import create_async_engine, AsyncSession
from sqlalchemy.orm import sessionmaker

# Add current directory to path for imports
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from models import Base, Agent, Execution, Log, Event, Metric

DATABASE_URL = "postgresql+asyncpg://postgres:postgres@postgres:5432/agentdb"

engine = create_async_engine(
    DATABASE_URL,
    echo=False,
    future=True,
)

AsyncSessionLocal = sessionmaker(
    engine, class_=AsyncSession, expire_on_commit=False, future=True
)


async def seed_data():
    """Insert sample agents and related data."""
    async with AsyncSessionLocal() as db:
        # Agent 1: Language Model Agent
        agent1 = Agent(
            id=uuid.uuid4(),
            name="LLM Agent",
            description="Language model inference agent using GPT-4",
            agent_type="langgraph",
            image="agent-runtime:latest",
            status="running",
            container_id="container_llm_001",
            container_name="agent-llm-001",
            cpu_limit=2,
            memory_limit="4GB",
            config={
                "model": "gpt-4",
                "temperature": 0.7,
                "max_tokens": 2048,
            },
            env_vars={
                "OPENAI_API_KEY": "sk-...",
                "DEBUG": "false",
            },
            labels={
                "team": "ai",
                "environment": "production",
                "version": "1.0",
            },
            created_at=datetime.utcnow(),
            updated_at=datetime.utcnow(),
        )

        # Agent 2: Data Processing Agent
        agent2 = Agent(
            id=uuid.uuid4(),
            name="Data Pipeline Agent",
            description="ETL and data transformation agent",
            agent_type="langgraph",
            image="agent-runtime:latest",
            status="running",
            container_id="container_data_001",
            container_name="agent-data-001",
            cpu_limit=4,
            memory_limit="8GB",
            config={
                "batch_size": 1000,
                "timeout_seconds": 300,
                "retry_attempts": 3,
            },
            env_vars={
                "DB_HOST": "postgres",
                "DB_PORT": "5432",
                "DEBUG": "true",
            },
            labels={
                "team": "data",
                "environment": "production",
                "version": "2.1",
            },
            created_at=datetime.utcnow() - timedelta(days=1),
            updated_at=datetime.utcnow() - timedelta(hours=2),
        )

        # Agent 3: Monitoring Agent
        agent3 = Agent(
            id=uuid.uuid4(),
            name="Monitoring Agent",
            description="System health and metrics monitoring",
            agent_type="langgraph",
            image="agent-runtime:latest",
            status="healthy",
            container_id="container_mon_001",
            container_name="agent-mon-001",
            cpu_limit=1,
            memory_limit="2GB",
            config={
                "check_interval": 30,
                "alert_threshold": 80,
            },
            env_vars={
                "PROMETHEUS_URL": "http://prometheus:9090",
            },
            labels={
                "team": "ops",
                "environment": "production",
            },
            created_at=datetime.utcnow() - timedelta(days=7),
            updated_at=datetime.utcnow() - timedelta(minutes=5),
        )

        # Add agents to session
        db.add(agent1)
        db.add(agent2)
        db.add(agent3)
        await db.flush()

        # Add sample executions
        exec1 = Execution(
            id=uuid.uuid4(),
            agent_id=agent1.id,
            status="completed",
            input={"prompt": "What is AI?", "max_tokens": 500},
            output={"text": "Artificial Intelligence is..."},
            error=None,
            started_at=datetime.utcnow() - timedelta(minutes=10),
            completed_at=datetime.utcnow() - timedelta(minutes=8),
            duration_ms=2000,
            exit_code=0,
        )

        exec2 = Execution(
            id=uuid.uuid4(),
            agent_id=agent2.id,
            status="running",
            input={"source": "s3://bucket/data.csv", "transform": "normalize"},
            output={},
            error=None,
            started_at=datetime.utcnow() - timedelta(minutes=2),
            completed_at=None,
            duration_ms=None,
            exit_code=None,
        )

        db.add(exec1)
        db.add(exec2)
        await db.flush()

        # Add sample logs
        log1 = Log(
            id=uuid.uuid4(),
            agent_id=agent1.id,
            execution_id=exec1.id,
            level="INFO",
            message="Processing request with prompt: What is AI?",
            source="llm_agent",
            meta={"request_id": "req-001", "model": "gpt-4"},
            timestamp=datetime.utcnow() - timedelta(minutes=10),
        )

        log2 = Log(
            id=uuid.uuid4(),
            agent_id=agent2.id,
            execution_id=exec2.id,
            level="DEBUG",
            message="Loading data chunk 1 of 50...",
            source="data_processor",
            meta={"chunk": 1, "rows": 1000},
            timestamp=datetime.utcnow() - timedelta(minutes=1),
        )

        log3 = Log(
            id=uuid.uuid4(),
            agent_id=agent3.id,
            execution_id=None,
            level="WARNING",
            message="CPU usage at 78% on agent-mon-001",
            source="monitor",
            meta={"current_cpu": 78, "threshold": 80},
            timestamp=datetime.utcnow() - timedelta(seconds=30),
        )

        db.add(log1)
        db.add(log2)
        db.add(log3)
        await db.flush()

        # Add sample events
        event1 = Event(
            id=uuid.uuid4(),
            agent_id=agent1.id,
            event_type="execution_started",
            payload={"execution_id": str(exec1.id)},
            source="agent_runtime",
            created_at=datetime.utcnow() - timedelta(minutes=10),
        )

        event2 = Event(
            id=uuid.uuid4(),
            agent_id=agent1.id,
            event_type="execution_completed",
            payload={"execution_id": str(exec1.id), "status": "completed"},
            source="agent_runtime",
            created_at=datetime.utcnow() - timedelta(minutes=8),
        )

        db.add(event1)
        db.add(event2)
        await db.flush()

        # Add sample metrics
        for i in range(5):
            metric = Metric(
                id=uuid.uuid4(),
                agent_id=agent1.id,
                metric_name="cpu_usage",
                metric_value=30 + (i * 5),
                labels={"unit": "percent", "host": "container_llm_001"},
                timestamp=datetime.utcnow() - timedelta(minutes=5 - i),
            )
            db.add(metric)

        for i in range(5):
            metric = Metric(
                id=uuid.uuid4(),
                agent_id=agent2.id,
                metric_name="memory_usage",
                metric_value=50 + (i * 8),
                labels={"unit": "percent", "host": "container_data_001"},
                timestamp=datetime.utcnow() - timedelta(minutes=5 - i),
            )
            db.add(metric)

        await db.commit()
        print("✓ Sample data seeded successfully!")
        print(f"  - Created 3 agents")
        print(f"  - Created 2 executions")
        print(f"  - Created 3 logs")
        print(f"  - Created 2 events")
        print(f"  - Created 10 metrics")

    await engine.dispose()


if __name__ == "__main__":
    asyncio.run(seed_data())
