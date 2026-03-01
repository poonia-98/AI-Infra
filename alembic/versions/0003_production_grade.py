"""production grade: nodes, schedules, orgs, rbac, execution enhancements

Revision ID: 0003_production_grade
Revises: 0002_alerts_containers
Create Date: 2026-01-01 00:00:00.000000
"""
from typing import Sequence, Union
from alembic import op
import sqlalchemy as sa
from sqlalchemy.dialects import postgresql

revision: str = '0003_production_grade'
down_revision: Union[str, None] = '0002_alerts_containers'
branch_labels: Union[str, Sequence[str], None] = None
depends_on: Union[str, Sequence[str], None] = None


def upgrade() -> None:
    # ── Organisations (multi-tenancy) ───────────────────────────────────
    op.create_table(
        'organisations',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('gen_random_uuid()')),
        sa.Column('name', sa.String(255), nullable=False, unique=True),
        sa.Column('slug', sa.String(100), nullable=False, unique=True),
        sa.Column('plan', sa.String(50), server_default='free'),
        sa.Column('max_agents', sa.Integer(), server_default='10'),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.func.now()),
    )

    # ── Executor nodes ──────────────────────────────────────────────────
    op.create_table(
        'executor_nodes',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('gen_random_uuid()')),
        sa.Column('node_id', sa.String(255), nullable=False, unique=True),
        sa.Column('hostname', sa.String(255), nullable=False),
        sa.Column('address', sa.String(255), nullable=False),
        sa.Column('port', sa.Integer(), server_default='8081'),
        sa.Column('status', sa.String(50), server_default='healthy'),
        sa.Column('capacity', sa.Integer(), server_default='50'),
        sa.Column('current_load', sa.Integer(), server_default='0'),
        sa.Column('last_heartbeat', sa.DateTime(timezone=True), server_default=sa.func.now()),
        sa.Column('metadata', postgresql.JSONB(), server_default='{}'),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.func.now()),
    )
    op.create_index('ix_executor_nodes_status', 'executor_nodes', ['status'])
    op.create_index('ix_executor_nodes_last_heartbeat', 'executor_nodes', ['last_heartbeat'])

    # ── Agent schedules ─────────────────────────────────────────────────
    op.create_table(
        'agent_schedules',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('gen_random_uuid()')),
        sa.Column('agent_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('agents.id', ondelete='CASCADE'), nullable=False),
        sa.Column('schedule_type', sa.String(20), nullable=False),   # cron | once | interval
        sa.Column('cron_expr', sa.String(100)),
        sa.Column('interval_seconds', sa.Integer()),
        sa.Column('run_at', sa.DateTime(timezone=True)),             # for one-time
        sa.Column('enabled', sa.Boolean(), server_default='true'),
        sa.Column('timezone', sa.String(100), server_default='UTC'),
        sa.Column('input', postgresql.JSONB(), server_default='{}'),
        sa.Column('last_run_at', sa.DateTime(timezone=True)),
        sa.Column('next_run_at', sa.DateTime(timezone=True)),
        sa.Column('run_count', sa.Integer(), server_default='0'),
        sa.Column('max_runs', sa.Integer()),                         # NULL = unlimited
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.func.now()),
        sa.Column('updated_at', sa.DateTime(timezone=True), server_default=sa.func.now()),
    )
    op.create_index('ix_agent_schedules_next_run_at', 'agent_schedules', ['next_run_at'])
    op.create_index('ix_agent_schedules_agent_id', 'agent_schedules', ['agent_id'])
    op.create_index('ix_agent_schedules_enabled', 'agent_schedules', ['enabled'])

    # ── Reconciliation events (audit log) ───────────────────────────────
    op.create_table(
        'reconciliation_events',
        sa.Column('id', postgresql.UUID(as_uuid=True), primary_key=True, server_default=sa.text('gen_random_uuid()')),
        sa.Column('agent_id', postgresql.UUID(as_uuid=True), sa.ForeignKey('agents.id', ondelete='SET NULL'), nullable=True),
        sa.Column('execution_id', postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column('node_id', sa.String(255)),
        sa.Column('case_type', sa.String(100), nullable=False),
        sa.Column('action_taken', sa.String(255)),
        sa.Column('before_state', postgresql.JSONB(), server_default='{}'),
        sa.Column('after_state', postgresql.JSONB(), server_default='{}'),
        sa.Column('created_at', sa.DateTime(timezone=True), server_default=sa.func.now()),
    )
    op.create_index('ix_reconciliation_events_agent_id', 'reconciliation_events', ['agent_id'])
    op.create_index('ix_reconciliation_events_created_at', 'reconciliation_events', ['created_at'])

    # ── Enhance executions table ────────────────────────────────────────
    try:
        op.add_column('executions', sa.Column('container_id', sa.String(255)))
        op.add_column('executions', sa.Column('node_id', sa.String(255)))
        op.add_column('executions', sa.Column('desired_state', sa.String(50), server_default='running'))
        op.add_column('executions', sa.Column('actual_state', sa.String(50), server_default='unknown'))
        op.add_column('executions', sa.Column('restart_policy', sa.String(20), server_default='never'))
        op.add_column('executions', sa.Column('restart_count', sa.Integer(), server_default='0'))
        op.add_column('executions', sa.Column('schedule_id', postgresql.UUID(as_uuid=True)))
    except Exception as e:
        print(f"executions columns may already exist: {e}")

    op.create_index('ix_executions_status', 'executions', ['status'])
    op.create_index('ix_executions_container_id', 'executions', ['container_id'])
    op.create_index('ix_executions_node_id', 'executions', ['node_id'])

    # ── Enhance agents table ────────────────────────────────────────────
    try:
        op.add_column('agents', sa.Column('node_id', sa.String(255)))
        op.add_column('agents', sa.Column('restart_policy', sa.String(20), server_default='never'))
        op.add_column('agents', sa.Column('desired_state', sa.String(50), server_default='stopped'))
        op.add_column('agents', sa.Column('organisation_id', postgresql.UUID(as_uuid=True)))
    except Exception as e:
        print(f"agents columns may already exist: {e}")


def downgrade() -> None:
    op.drop_table('reconciliation_events')
    op.drop_table('agent_schedules')
    op.drop_table('executor_nodes')
    op.drop_table('organisations')