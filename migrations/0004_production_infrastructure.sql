-- =============================================================================
--  AI Agent Infrastructure Platform
--  Migration: 0004_production_infrastructure
--
--  Adds ALL missing production-grade tables.
--  100% idempotent — safe to run multiple times on existing databases.
--  Preserves all existing data.
--
--  New tables:
--    nodes                 node registration + capacity
--    node_heartbeats       time-series heartbeat records
--    service_leaders       distributed leader election
--    agent_logs            high-throughput container log persistence
--    execution_logs        execution-scoped log view
--    configs               centralized config management
--    secrets               encrypted secret storage
--    audit_logs            immutable platform action audit trail
--    execution_queue       distributed execution coordination
--    organisations         multi-tenancy (was added in 0003 for SQLAlchemy;
--                          this script ensures it exists in raw SQL deploys)
--    agent_schedules       cron/interval/once scheduling
--    reconciliation_events reconciliation audit trail
-- =============================================================================

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
--  Extensions
-- ─────────────────────────────────────────────────────────────────────────────
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";   -- for gen_random_bytes / pgp_sym_encrypt
CREATE EXTENSION IF NOT EXISTS "pg_stat_statements";

-- ─────────────────────────────────────────────────────────────────────────────
--  Shared trigger function (idempotent)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
--  Helper macro: safely attach the updated_at trigger
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION ensure_updated_at_trigger(p_table TEXT)
RETURNS VOID AS $$
BEGIN
  EXECUTE format(
    'DROP TRIGGER IF EXISTS set_%I_updated_at ON %I;
     CREATE TRIGGER set_%I_updated_at
       BEFORE UPDATE ON %I
       FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();',
    p_table, p_table, p_table, p_table
  );
END;
$$ LANGUAGE plpgsql;


-- =============================================================================
--  Backfill columns on EXISTING tables (idempotent ALTER TABLE)
-- =============================================================================

-- agents: reconciliation + control-plane fields added in 0003
ALTER TABLE agents ADD COLUMN IF NOT EXISTS desired_state      VARCHAR(50)  NOT NULL DEFAULT 'stopped';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS actual_state       VARCHAR(50)  NOT NULL DEFAULT 'unknown';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS restart_policy     VARCHAR(50)  NOT NULL DEFAULT 'never';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS node_id            VARCHAR(255);
ALTER TABLE agents ADD COLUMN IF NOT EXISTS organisation_id    UUID;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS last_reconciled_at TIMESTAMPTZ;

-- executions: execution-coordination fields
ALTER TABLE executions ADD COLUMN IF NOT EXISTS desired_state  VARCHAR(50)  NOT NULL DEFAULT 'running';
ALTER TABLE executions ADD COLUMN IF NOT EXISTS actual_state   VARCHAR(50)  NOT NULL DEFAULT 'unknown';
ALTER TABLE executions ADD COLUMN IF NOT EXISTS container_id   VARCHAR(255);
ALTER TABLE executions ADD COLUMN IF NOT EXISTS node_id        VARCHAR(255);
ALTER TABLE executions ADD COLUMN IF NOT EXISTS restart_policy VARCHAR(20)  NOT NULL DEFAULT 'never';
ALTER TABLE executions ADD COLUMN IF NOT EXISTS restart_count  INTEGER      NOT NULL DEFAULT 0;
ALTER TABLE executions ADD COLUMN IF NOT EXISTS schedule_id    UUID;
ALTER TABLE executions ADD COLUMN IF NOT EXISTS locked_by      VARCHAR(255);
ALTER TABLE executions ADD COLUMN IF NOT EXISTS locked_at      TIMESTAMPTZ;

-- users: role + org membership
ALTER TABLE users ADD COLUMN IF NOT EXISTS role            VARCHAR(50) NOT NULL DEFAULT 'developer';
ALTER TABLE users ADD COLUMN IF NOT EXISTS organisation_id UUID;


-- =============================================================================
--  1. organisations
-- =============================================================================
CREATE TABLE IF NOT EXISTS organisations (
    id         UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    name       VARCHAR(255) NOT NULL UNIQUE,
    slug       VARCHAR(100) NOT NULL UNIQUE,
    plan       VARCHAR(50)  NOT NULL DEFAULT 'free',
    max_agents INTEGER      NOT NULL DEFAULT 10,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_organisations_slug ON organisations(slug);


-- =============================================================================
--  2. executor_nodes  (canonical node registry)
--     Tracks every executor instance that registers with the control plane.
-- =============================================================================
CREATE TABLE IF NOT EXISTS executor_nodes (
    id             UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    node_id        VARCHAR(255) NOT NULL UNIQUE,
    hostname       VARCHAR(255) NOT NULL DEFAULT '',
    address        VARCHAR(255) NOT NULL DEFAULT '',
    port           INTEGER      NOT NULL DEFAULT 8081,

    -- Status: healthy | degraded | dead | draining
    status         VARCHAR(50)  NOT NULL DEFAULT 'healthy',

    -- Capacity bookkeeping
    capacity       INTEGER      NOT NULL DEFAULT 50,
    current_load   INTEGER      NOT NULL DEFAULT 0,

    -- Last seen timestamp — drives dead-node detection
    last_heartbeat TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    metadata       JSONB        NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_executor_nodes_status         ON executor_nodes(status);
CREATE INDEX IF NOT EXISTS idx_executor_nodes_last_heartbeat ON executor_nodes(last_heartbeat DESC);
SELECT ensure_updated_at_trigger('executor_nodes');


-- =============================================================================
--  3. node_heartbeats  (time-series heartbeat records)
--     Every heartbeat is appended here for capacity trending + forensics.
--     Old rows can be purged via a retention job (default: keep 7 days).
-- =============================================================================
CREATE TABLE IF NOT EXISTS node_heartbeats (
    id          UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    node_id     VARCHAR(255) NOT NULL,
    status      VARCHAR(50)  NOT NULL DEFAULT 'healthy',
    cpu_percent FLOAT        NOT NULL DEFAULT 0,
    memory_mb   FLOAT        NOT NULL DEFAULT 0,
    load        INTEGER      NOT NULL DEFAULT 0,
    capacity    INTEGER      NOT NULL DEFAULT 50,
    metadata    JSONB        NOT NULL DEFAULT '{}',
    received_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_node_heartbeats_node_id     ON node_heartbeats(node_id);
CREATE INDEX IF NOT EXISTS idx_node_heartbeats_received_at ON node_heartbeats(received_at DESC);
-- Partial index for live nodes only (most queries)
CREATE INDEX IF NOT EXISTS idx_node_heartbeats_node_recent
    ON node_heartbeats(node_id, received_at DESC)
    WHERE received_at > NOW() - INTERVAL '1 hour';


-- =============================================================================
--  4. service_leaders  (PostgreSQL advisory-lock-backed leader election)
--     One row per service name. The holder updates acquired_at every TTL/2.
--     A challenger wins if NOW() - acquired_at > ttl_seconds.
-- =============================================================================
CREATE TABLE IF NOT EXISTS service_leaders (
    service_name VARCHAR(100) PRIMARY KEY,
    leader_id    VARCHAR(255) NOT NULL,
    acquired_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    ttl_seconds  INTEGER      NOT NULL DEFAULT 30,
    metadata     JSONB        NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_service_leaders_acquired_at ON service_leaders(acquired_at DESC);


-- =============================================================================
--  5. agent_logs  (high-throughput container log persistence)
--     Pipeline: Executor → NATS → log-processor → here
--     Separate from the existing `logs` table to keep telemetry logs isolated
--     from raw container stdout/stderr.
-- =============================================================================
CREATE TABLE IF NOT EXISTS agent_logs (
    id           UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id     UUID         NOT NULL REFERENCES agents(id)     ON DELETE CASCADE,
    execution_id UUID                     REFERENCES executions(id) ON DELETE SET NULL,

    -- stream: stdout | stderr
    stream       VARCHAR(10)  NOT NULL DEFAULT 'stdout',
    -- level: debug | info | warn | error
    level        VARCHAR(20)  NOT NULL DEFAULT 'info',
    message      TEXT         NOT NULL,
    source       VARCHAR(100) NOT NULL DEFAULT 'container',
    container_id VARCHAR(255),
    metadata     JSONB        NOT NULL DEFAULT '{}',

    logged_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Composite covering indexes for the two most common query shapes:
--   1. agent timeline:  WHERE agent_id = ? ORDER BY logged_at DESC LIMIT n
--   2. execution logs:  WHERE execution_id = ? ORDER BY logged_at ASC
CREATE INDEX IF NOT EXISTS idx_agent_logs_agent_time     ON agent_logs(agent_id, logged_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_logs_execution_time ON agent_logs(execution_id, logged_at ASC)
    WHERE execution_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_logs_level          ON agent_logs(level)
    WHERE level IN ('error','warn');
CREATE INDEX IF NOT EXISTS idx_agent_logs_logged_at      ON agent_logs(logged_at DESC);


-- =============================================================================
--  6. execution_logs  (execution-scoped log summary / cursor table)
--     Lightweight table for fast "give me all logs for execution X" queries
--     without a full agent_logs scan.  The log-processor writes both tables.
-- =============================================================================
CREATE TABLE IF NOT EXISTS execution_logs (
    id           UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id UUID        NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    agent_id     UUID        NOT NULL REFERENCES agents(id)     ON DELETE CASCADE,
    log_count    INTEGER     NOT NULL DEFAULT 0,
    error_count  INTEGER     NOT NULL DEFAULT 0,
    warn_count   INTEGER     NOT NULL DEFAULT 0,
    first_log_at TIMESTAMPTZ,
    last_log_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (execution_id)
);

CREATE INDEX IF NOT EXISTS idx_execution_logs_execution ON execution_logs(execution_id);
CREATE INDEX IF NOT EXISTS idx_execution_logs_agent     ON execution_logs(agent_id);
SELECT ensure_updated_at_trigger('execution_logs');


-- =============================================================================
--  7. configs  (centralised config management with versioning)
-- =============================================================================
CREATE TABLE IF NOT EXISTS configs (
    id          UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    service     VARCHAR(100) NOT NULL,   -- e.g. "scheduler", "reconciliation", "global"
    key         VARCHAR(255) NOT NULL,
    value       TEXT         NOT NULL,
    value_type  VARCHAR(20)  NOT NULL DEFAULT 'string',  -- string | int | bool | json
    description TEXT,
    version     INTEGER      NOT NULL DEFAULT 1,
    is_secret   BOOLEAN      NOT NULL DEFAULT FALSE,
    created_by  VARCHAR(255),
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    UNIQUE (service, key)
);

CREATE INDEX IF NOT EXISTS idx_configs_service ON configs(service);
CREATE INDEX IF NOT EXISTS idx_configs_key     ON configs(service, key);
SELECT ensure_updated_at_trigger('configs');

-- Config version history (append-only audit trail for config changes)
CREATE TABLE IF NOT EXISTS config_history (
    id          UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    config_id   UUID         NOT NULL REFERENCES configs(id) ON DELETE CASCADE,
    service     VARCHAR(100) NOT NULL,
    key         VARCHAR(255) NOT NULL,
    old_value   TEXT,
    new_value   TEXT         NOT NULL,
    version     INTEGER      NOT NULL,
    changed_by  VARCHAR(255),
    changed_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_config_history_config_id  ON config_history(config_id);
CREATE INDEX IF NOT EXISTS idx_config_history_changed_at ON config_history(changed_at DESC);


-- =============================================================================
--  8. secrets  (encrypted secret storage)
--     Values are encrypted with pgcrypto pgp_sym_encrypt.
--     The encryption passphrase is stored as the env var SECRETS_PASSPHRASE.
--     This table stores ciphertext only — never plaintext.
-- =============================================================================
CREATE TABLE IF NOT EXISTS secrets (
    id           UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    service      VARCHAR(100) NOT NULL,  -- owning service, e.g. "agent", "backend"
    name         VARCHAR(255) NOT NULL,  -- human-readable key
    -- encrypted_value holds pgp_sym_encrypt(plaintext, passphrase)
    encrypted_value BYTEA     NOT NULL,
    algorithm    VARCHAR(50)  NOT NULL DEFAULT 'pgp_sym',
    description  TEXT,
    version      INTEGER      NOT NULL DEFAULT 1,
    rotated_at   TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    created_by   VARCHAR(255),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    UNIQUE (service, name)
);

CREATE INDEX IF NOT EXISTS idx_secrets_service    ON secrets(service);
CREATE INDEX IF NOT EXISTS idx_secrets_expires_at ON secrets(expires_at)
    WHERE expires_at IS NOT NULL;
SELECT ensure_updated_at_trigger('secrets');


-- =============================================================================
--  9. audit_logs  (immutable platform action audit trail)
--     append-only — no UPDATE/DELETE permitted at the application layer.
--     Row-level security should be applied in Supabase for extra safety.
-- =============================================================================
CREATE TABLE IF NOT EXISTS audit_logs (
    id          UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Actor (who did it)
    actor_id    VARCHAR(255),          -- user UUID or service name
    actor_type  VARCHAR(50)  NOT NULL DEFAULT 'user',  -- user | service | system
    actor_ip    INET,

    -- Action
    action      VARCHAR(100) NOT NULL, -- agent.created | agent.started | rbac.role_changed …
    resource    VARCHAR(100),          -- resource type: agent | execution | schedule …
    resource_id UUID,

    -- Payload (what changed)
    old_value   JSONB        NOT NULL DEFAULT '{}',
    new_value   JSONB        NOT NULL DEFAULT '{}',
    metadata    JSONB        NOT NULL DEFAULT '{}',

    -- Context
    request_id  VARCHAR(100),
    service     VARCHAR(100) NOT NULL DEFAULT 'backend',
    status      VARCHAR(20)  NOT NULL DEFAULT 'success',  -- success | failure
    error       TEXT,

    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Compound indexes to support the most common audit queries
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor        ON audit_logs(actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action       ON audit_logs(action, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_resource     ON audit_logs(resource, resource_id, created_at DESC)
    WHERE resource_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at   ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_service      ON audit_logs(service, created_at DESC);

-- Prevent accidental application-layer deletes/updates (belt-and-suspenders)
CREATE OR REPLACE RULE audit_logs_no_update AS
    ON UPDATE TO audit_logs DO INSTEAD NOTHING;
CREATE OR REPLACE RULE audit_logs_no_delete AS
    ON DELETE TO audit_logs DO INSTEAD NOTHING;


-- =============================================================================
--  10. execution_queue  (distributed execution coordination)
--     Redis handles real-time locking; this table is the durable fallback and
--     the source of truth for "what should run on which node".
-- =============================================================================
CREATE TABLE IF NOT EXISTS execution_queue (
    id           UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id UUID         NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    agent_id     UUID         NOT NULL REFERENCES agents(id)     ON DELETE CASCADE,

    -- Queue state: pending | claimed | running | completed | failed | cancelled
    status       VARCHAR(50)  NOT NULL DEFAULT 'pending',

    -- Node assignment
    assigned_node VARCHAR(255),
    priority      INTEGER      NOT NULL DEFAULT 5,  -- 1 (highest) to 10 (lowest)

    -- Locking (prevents duplicate execution across nodes)
    locked_by    VARCHAR(255),          -- node_id that holds the lock
    locked_at    TIMESTAMPTZ,
    lock_expires TIMESTAMPTZ,           -- lock TTL: locked_at + 30s

    -- Scheduling
    scheduled_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,

    -- Retry tracking
    attempt      INTEGER      NOT NULL DEFAULT 1,
    max_attempts INTEGER      NOT NULL DEFAULT 3,
    last_error   TEXT,

    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    UNIQUE (execution_id)  -- one queue entry per execution
);

-- Covering index for the queue worker's hot path:
--   SELECT * FROM execution_queue
--   WHERE status='pending' AND (lock_expires IS NULL OR lock_expires < NOW())
--   ORDER BY priority ASC, scheduled_at ASC
--   FOR UPDATE SKIP LOCKED
CREATE INDEX IF NOT EXISTS idx_exec_queue_pending
    ON execution_queue(priority ASC, scheduled_at ASC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_exec_queue_node    ON execution_queue(assigned_node) WHERE assigned_node IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_exec_queue_status  ON execution_queue(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_queue_agent   ON execution_queue(agent_id);
SELECT ensure_updated_at_trigger('execution_queue');


-- =============================================================================
--  11. agent_schedules  (cron / interval / once scheduling)
-- =============================================================================
CREATE TABLE IF NOT EXISTS agent_schedules (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id         UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    schedule_type    VARCHAR(20)  NOT NULL DEFAULT 'cron',  -- cron | once | interval
    cron_expr        VARCHAR(100),
    interval_seconds INTEGER,
    run_at           TIMESTAMPTZ,
    enabled          BOOLEAN      NOT NULL DEFAULT TRUE,
    timezone         VARCHAR(100) NOT NULL DEFAULT 'UTC',
    input            JSONB        NOT NULL DEFAULT '{}',
    last_run_at      TIMESTAMPTZ,
    next_run_at      TIMESTAMPTZ,
    run_count        INTEGER      NOT NULL DEFAULT 0,
    max_runs         INTEGER,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_agent_schedules_agent_id    ON agent_schedules(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_schedules_next_run_at ON agent_schedules(next_run_at ASC)
    WHERE enabled = TRUE;
SELECT ensure_updated_at_trigger('agent_schedules');


-- =============================================================================
--  12. reconciliation_events  (reconciliation audit trail)
-- =============================================================================
CREATE TABLE IF NOT EXISTS reconciliation_events (
    id           UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id     UUID         REFERENCES agents(id)     ON DELETE SET NULL,
    execution_id UUID         REFERENCES executions(id) ON DELETE SET NULL,
    node_id      VARCHAR(255),
    case_type    VARCHAR(100) NOT NULL,
    action_taken VARCHAR(255),
    before_state JSONB        NOT NULL DEFAULT '{}',
    after_state  JSONB        NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_agent      ON reconciliation_events(agent_id, created_at DESC)
    WHERE agent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_reconciliation_created_at ON reconciliation_events(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_reconciliation_case_type  ON reconciliation_events(case_type);


-- =============================================================================
--  13. schedule_executions  (link schedule run → execution)
-- =============================================================================
CREATE TABLE IF NOT EXISTS schedule_executions (
    id           UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    schedule_id  UUID         NOT NULL REFERENCES agent_schedules(id) ON DELETE CASCADE,
    agent_id     UUID         NOT NULL REFERENCES agents(id)          ON DELETE CASCADE,
    execution_id UUID                     REFERENCES executions(id)   ON DELETE SET NULL,
    status       VARCHAR(50)  NOT NULL DEFAULT 'pending',
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    error        TEXT,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_schedule_executions_schedule ON schedule_executions(schedule_id);
CREATE INDEX IF NOT EXISTS idx_schedule_executions_status   ON schedule_executions(status);
CREATE INDEX IF NOT EXISTS idx_schedule_executions_agent    ON schedule_executions(agent_id);


-- =============================================================================
--  14. reconciliation_state  (desired vs actual per execution)
-- =============================================================================
CREATE TABLE IF NOT EXISTS reconciliation_state (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id    UUID         REFERENCES executions(id) ON DELETE CASCADE,
    agent_id        UUID         REFERENCES agents(id)     ON DELETE CASCADE,
    container_id    VARCHAR(255),
    desired_state   VARCHAR(50),
    actual_state    VARCHAR(50),
    last_checked_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_state_execution ON reconciliation_state(execution_id);
CREATE INDEX IF NOT EXISTS idx_reconciliation_state_agent     ON reconciliation_state(agent_id);
SELECT ensure_updated_at_trigger('reconciliation_state');


-- =============================================================================
--  SEED: default configs
-- =============================================================================
INSERT INTO configs (service, key, value, value_type, description)
VALUES
    ('global',          'log_retention_days',     '30',    'int',    'How many days to keep agent_logs rows'),
    ('global',          'heartbeat_ttl_seconds',  '30',    'int',    'Seconds before a node is considered dead'),
    ('global',          'max_restart_count',      '5',     'int',    'Max automatic restarts before marking agent as error'),
    ('scheduler',       'tick_interval_seconds',  '10',    'int',    'How often the scheduler polls for due schedules'),
    ('reconciliation',  'interval_seconds',        '5',    'int',    'How often the reconciliation loop runs'),
    ('reconciliation',  'stuck_starting_ttl_sec', '120',   'int',    'Seconds before a starting execution is marked failed'),
    ('execution_queue', 'lock_ttl_seconds',        '30',   'int',    'Lock TTL for execution queue entries'),
    ('execution_queue', 'max_concurrent_per_node', '20',   'int',    'Max concurrent executions per executor node')
ON CONFLICT (service, key) DO NOTHING;


-- =============================================================================
--  SEED: default service leader rows (facilitates upsert pattern)
-- =============================================================================
INSERT INTO service_leaders (service_name, leader_id, ttl_seconds)
VALUES
    ('scheduler',        'unelected', 30),
    ('reconciliation',   'unelected', 30)
ON CONFLICT (service_name) DO NOTHING;


-- =============================================================================
--  Verification
-- =============================================================================
SELECT
    t.table_name,
    col.column_count,
    COALESCE(idx.index_count, 0) AS index_count
FROM information_schema.tables t
JOIN (
    SELECT table_name, COUNT(*) AS column_count
    FROM information_schema.columns
    WHERE table_schema = 'public'
    GROUP BY table_name
) col USING (table_name)
LEFT JOIN (
    SELECT tablename AS table_name, COUNT(*) AS index_count
    FROM pg_indexes
    WHERE schemaname = 'public'
    GROUP BY tablename
) idx USING (table_name)
WHERE t.table_schema = 'public'
  AND t.table_type   = 'BASE TABLE'
ORDER BY t.table_name;

COMMIT;