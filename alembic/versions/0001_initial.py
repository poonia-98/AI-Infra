"""initial schema

Revision ID: 0001_initial
Revises:
Create Date: 2025-01-01 00:00:00.000000

"""
from typing import Sequence, Union
from alembic import op
import sqlalchemy as sa
from sqlalchemy.dialects import postgresql

revision: str = '0001_initial'
down_revision: Union[str, None] = None
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    op.execute('CREATE EXTENSION IF NOT EXISTS "uuid-ossp"')

    op.create_table(
        'users',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('uuid_generate_v4()')),
        sa.Column('username', sa.String(255), nullable=False, unique=True),
        sa.Column('email', sa.String(255), nullable=False, unique=True),
        sa.Column('hashed_password', sa.String(255), nullable=False),
        sa.Column('is_active', sa.Boolean(), server_default='true'),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.text('now()')),
        sa.Column('updated_at', sa.DateTime(timezone=True), server_default=sa.text('now()')),
    )

    op.create_table(
        'agents',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('uuid_generate_v4()')),
        sa.Column('name', sa.String(255), nullable=False),
        sa.Column('description', sa.Text()),
        sa.Column('agent_type', sa.String(100), server_default='langgraph'),
        sa.Column('config', postgresql.JSONB(), server_default='{}'),
        sa.Column('image', sa.String(255), server_default='agent-runtime:latest'),
        sa.Column('status', sa.String(50), server_default='created'),
        sa.Column('container_id', sa.String(255)),
        sa.Column('container_name', sa.String(255)),
        sa.Column('cpu_limit', sa.Float(), server_default='1.0'),
        sa.Column('memory_limit', sa.String(20), server_default='512m'),
        sa.Column('env_vars', postgresql.JSONB(), server_default='{}'),
        sa.Column('labels', postgresql.JSONB(), server_default='{}'),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.text('now()')),
        sa.Column('updated_at', sa.DateTime(timezone=True), server_default=sa.text('now()')),
    )

    op.create_table(
        'executions',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('uuid_generate_v4()')),
        sa.Column('agent_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('agents.id', ondelete='CASCADE'), nullable=False),
        sa.Column('status', sa.String(50), server_default='pending'),
        sa.Column('input', postgresql.JSONB(), server_default='{}'),
        sa.Column('output', postgresql.JSONB(), server_default='{}'),
        sa.Column('error', sa.Text()),
        sa.Column('started_at', sa.DateTime(timezone=True)),
        sa.Column('completed_at', sa.DateTime(timezone=True)),
        sa.Column('duration_ms', sa.BigInteger()),
        sa.Column('exit_code', sa.Integer()),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.text('now()')),
    )

    op.create_table(
        'logs',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('uuid_generate_v4()')),
        sa.Column('agent_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('agents.id', ondelete='CASCADE'), nullable=False),
        sa.Column('execution_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('executions.id', ondelete='SET NULL')),
        sa.Column('level', sa.String(20), server_default='info'),
        sa.Column('message', sa.Text(), nullable=False),
        sa.Column('source', sa.String(100)),
        sa.Column('metadata', postgresql.JSONB(), server_default='{}'),
        sa.Column('timestamp', sa.DateTime(timezone=True), server_default=sa.text('now()')),
    )

    op.create_table(
        'events',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('uuid_generate_v4()')),
        sa.Column('agent_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('agents.id', ondelete='CASCADE'), nullable=False),
        sa.Column('execution_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('executions.id', ondelete='SET NULL')),
        sa.Column('event_type', sa.String(100), nullable=False),
        sa.Column('payload', postgresql.JSONB(), server_default='{}'),
        sa.Column('source', sa.String(100)),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.text('now()')),
    )

    op.create_table(
        'metrics',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('uuid_generate_v4()')),
        sa.Column('agent_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('agents.id', ondelete='CASCADE'), nullable=False),
        sa.Column('metric_name', sa.String(100), nullable=False),
        sa.Column('metric_value', sa.Float(), nullable=False),
        sa.Column('labels', postgresql.JSONB(), server_default='{}'),
        sa.Column('timestamp', sa.DateTime(timezone=True), server_default=sa.text('now()')),
    )

    # Indexes
    op.create_index('idx_executions_agent_id', 'executions', ['agent_id'])
    op.create_index('idx_executions_status', 'executions', ['status'])
    op.create_index('idx_logs_agent_id', 'logs', ['agent_id'])
    op.create_index('idx_logs_timestamp', 'logs', ['timestamp'], postgresql_ops={'timestamp': 'DESC'})
    op.create_index('idx_events_agent_id', 'events', ['agent_id'])
    op.create_index('idx_events_created_at', 'events', ['created_at'], postgresql_ops={'created_at': 'DESC'})
    op.create_index('idx_metrics_agent_id', 'metrics', ['agent_id'])
    op.create_index('idx_metrics_timestamp', 'metrics', ['timestamp'], postgresql_ops={'timestamp': 'DESC'})
    op.create_index('idx_agents_status', 'agents', ['status'])


def downgrade() -> None:
    op.drop_table('metrics')
    op.drop_table('events')
    op.drop_table('logs')
    op.drop_table('executions')
    op.drop_table('agents')
    op.drop_table('users')