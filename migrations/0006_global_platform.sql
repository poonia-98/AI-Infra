-- =============================================================================
--  0006_global_platform.sql
--  AI Agent Cloud Platform — Global infrastructure tables
--  Idempotent. Safe to rerun.
-- =============================================================================
BEGIN;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "vector";      -- pgvector for memory embeddings

CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$ BEGIN NEW.updated_at = NOW(); RETURN NEW; END; $$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
--  AGENT VERSIONING
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS agent_versions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    version         VARCHAR(50)  NOT NULL,           -- semver: 1.0.0
    image           VARCHAR(500) NOT NULL,
    config          JSONB        NOT NULL DEFAULT '{}',
    env_vars        JSONB        NOT NULL DEFAULT '{}',
    labels          JSONB        NOT NULL DEFAULT '{}',
    cpu_limit       FLOAT        NOT NULL DEFAULT 1.0,
    memory_limit    VARCHAR(20)  NOT NULL DEFAULT '512m',
    status          VARCHAR(30)  NOT NULL DEFAULT 'inactive',
    -- deployment strategy: rolling | blue_green | canary
    deploy_strategy VARCHAR(30)  NOT NULL DEFAULT 'rolling',
    canary_weight   INTEGER      NOT NULL DEFAULT 0,  -- 0-100 percent
    is_current      BOOLEAN      NOT NULL DEFAULT FALSE,
    changelog       TEXT,
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (agent_id, version)
);
CREATE INDEX IF NOT EXISTS idx_agent_versions_agent    ON agent_versions(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_versions_current  ON agent_versions(agent_id, is_current) WHERE is_current;

-- ─────────────────────────────────────────────────────────────────────────────
--  WORKFLOW ENGINE
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS workflows (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         REFERENCES organisations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    -- DAG definition stored as JSON adjacency list
    dag             JSONB        NOT NULL DEFAULT '{"nodes":[],"edges":[]}',
    -- trigger: manual | schedule | event | webhook
    trigger_type    VARCHAR(30)  NOT NULL DEFAULT 'manual',
    trigger_config  JSONB        NOT NULL DEFAULT '{}',
    status          VARCHAR(30)  NOT NULL DEFAULT 'draft',  -- draft|active|paused|archived
    version         INTEGER      NOT NULL DEFAULT 1,
    max_retries     INTEGER      NOT NULL DEFAULT 3,
    timeout_seconds INTEGER      NOT NULL DEFAULT 3600,
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_workflows_org    ON workflows(organisation_id);
CREATE INDEX IF NOT EXISTS idx_workflows_status ON workflows(status);
DROP TRIGGER IF EXISTS set_workflows_updated_at ON workflows;
CREATE TRIGGER set_workflows_updated_at BEFORE UPDATE ON workflows FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS workflow_executions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id     UUID         NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- status: pending|running|completed|failed|cancelled|timed_out
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    input           JSONB        NOT NULL DEFAULT '{}',
    output          JSONB        NOT NULL DEFAULT '{}',
    context         JSONB        NOT NULL DEFAULT '{}',   -- shared state between nodes
    error           TEXT,
    triggered_by    VARCHAR(100),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    duration_ms     BIGINT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_wf_exec_workflow ON workflow_executions(workflow_id);
CREATE INDEX IF NOT EXISTS idx_wf_exec_status   ON workflow_executions(status);
CREATE INDEX IF NOT EXISTS idx_wf_exec_created  ON workflow_executions(created_at DESC);
DROP TRIGGER IF EXISTS set_wf_exec_updated_at ON workflow_executions;
CREATE TRIGGER set_wf_exec_updated_at BEFORE UPDATE ON workflow_executions FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS workflow_node_executions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_exec_id UUID        NOT NULL REFERENCES workflow_executions(id) ON DELETE CASCADE,
    node_id         VARCHAR(255) NOT NULL,   -- references dag.nodes[].id
    node_type       VARCHAR(50)  NOT NULL,   -- agent|condition|transform|wait|webhook
    agent_id        UUID         REFERENCES agents(id) ON DELETE SET NULL,
    execution_id    UUID         REFERENCES executions(id) ON DELETE SET NULL,
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    input           JSONB        NOT NULL DEFAULT '{}',
    output          JSONB        NOT NULL DEFAULT '{}',
    error           TEXT,
    attempt         INTEGER      NOT NULL DEFAULT 1,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    duration_ms     BIGINT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_wf_node_exec_wf   ON workflow_node_executions(workflow_exec_id);
CREATE INDEX IF NOT EXISTS idx_wf_node_exec_node ON workflow_node_executions(node_id);
CREATE INDEX IF NOT EXISTS idx_wf_node_exec_status ON workflow_node_executions(status);

-- ─────────────────────────────────────────────────────────────────────────────
--  EXECUTION QUEUES (priority, retry, dead-letter)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS execution_queues (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         REFERENCES organisations(id) ON DELETE CASCADE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    workflow_exec_id UUID        REFERENCES workflow_executions(id) ON DELETE SET NULL,
    -- queue_type: standard | priority | retry | dead_letter
    queue_type      VARCHAR(30)  NOT NULL DEFAULT 'standard',
    priority        INTEGER      NOT NULL DEFAULT 5,  -- 1 (highest) to 10
    status          VARCHAR(30)  NOT NULL DEFAULT 'queued',
    input           JSONB        NOT NULL DEFAULT '{}',
    assigned_node   VARCHAR(255),
    locked_by       VARCHAR(255),
    locked_at       TIMESTAMPTZ,
    lock_expires_at TIMESTAMPTZ,
    attempt         INTEGER      NOT NULL DEFAULT 1,
    max_attempts    INTEGER      NOT NULL DEFAULT 3,
    last_error      TEXT,
    scheduled_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_exec_queues_priority ON execution_queues(priority ASC, scheduled_at ASC) WHERE status='queued';
CREATE INDEX IF NOT EXISTS idx_exec_queues_status   ON execution_queues(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_queues_org      ON execution_queues(organisation_id);
CREATE INDEX IF NOT EXISTS idx_exec_queues_agent    ON execution_queues(agent_id);
DROP TRIGGER IF EXISTS set_exec_queues_updated_at ON execution_queues;
CREATE TRIGGER set_exec_queues_updated_at BEFORE UPDATE ON execution_queues FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
--  AGENT MARKETPLACE
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS agent_templates (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    name            VARCHAR(255) NOT NULL,
    slug            VARCHAR(255) NOT NULL UNIQUE,
    description     TEXT,
    category        VARCHAR(100),
    tags            TEXT[]       NOT NULL DEFAULT '{}',
    agent_type      VARCHAR(100) NOT NULL DEFAULT 'langgraph',
    image           VARCHAR(500) NOT NULL,
    config_schema   JSONB        NOT NULL DEFAULT '{}',   -- JSON Schema for config
    default_config  JSONB        NOT NULL DEFAULT '{}',
    default_env     JSONB        NOT NULL DEFAULT '{}',
    cpu_limit       FLOAT        NOT NULL DEFAULT 1.0,
    memory_limit    VARCHAR(20)  NOT NULL DEFAULT '512m',
    is_public       BOOLEAN      NOT NULL DEFAULT FALSE,
    is_verified     BOOLEAN      NOT NULL DEFAULT FALSE,
    version         VARCHAR(50)  NOT NULL DEFAULT '1.0.0',
    downloads       INTEGER      NOT NULL DEFAULT 0,
    rating          FLOAT        NOT NULL DEFAULT 0,
    rating_count    INTEGER      NOT NULL DEFAULT 0,
    readme          TEXT,
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_templates_slug     ON agent_templates(slug);
CREATE INDEX IF NOT EXISTS idx_agent_templates_category ON agent_templates(category);
CREATE INDEX IF NOT EXISTS idx_agent_templates_public   ON agent_templates(is_public) WHERE is_public;
CREATE INDEX IF NOT EXISTS idx_agent_templates_tags     ON agent_templates USING GIN(tags);
DROP TRIGGER IF EXISTS set_agent_templates_updated_at ON agent_templates;
CREATE TRIGGER set_agent_templates_updated_at BEFORE UPDATE ON agent_templates FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS agent_registry (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    template_id     UUID         NOT NULL REFERENCES agent_templates(id) ON DELETE CASCADE,
    version         VARCHAR(50)  NOT NULL,
    image           VARCHAR(500) NOT NULL,
    image_digest    VARCHAR(255),
    config          JSONB        NOT NULL DEFAULT '{}',
    changelog       TEXT,
    is_latest       BOOLEAN      NOT NULL DEFAULT FALSE,
    published_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (template_id, version)
);
CREATE INDEX IF NOT EXISTS idx_agent_registry_template ON agent_registry(template_id);
CREATE INDEX IF NOT EXISTS idx_agent_registry_latest   ON agent_registry(template_id, is_latest) WHERE is_latest;

CREATE TABLE IF NOT EXISTS template_reviews (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    template_id     UUID         NOT NULL REFERENCES agent_templates(id) ON DELETE CASCADE,
    user_id         UUID         REFERENCES users(id) ON DELETE SET NULL,
    rating          INTEGER      NOT NULL CHECK (rating BETWEEN 1 AND 5),
    review          TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_template_reviews_template ON template_reviews(template_id);

-- ─────────────────────────────────────────────────────────────────────────────
--  AGENT MEMORY LAYER
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS agent_memory (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- memory_type: short_term | long_term | episodic | semantic | procedural
    memory_type     VARCHAR(30)  NOT NULL DEFAULT 'short_term',
    key             VARCHAR(500),
    content         TEXT         NOT NULL,
    -- pgvector embedding (1536-dim for OpenAI, 768-dim for others)
    embedding       vector(1536),
    metadata        JSONB        NOT NULL DEFAULT '{}',
    importance      FLOAT        NOT NULL DEFAULT 0.5,   -- 0-1 decay weight
    access_count    INTEGER      NOT NULL DEFAULT 0,
    last_accessed   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,
    session_id      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_memory_agent       ON agent_memory(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_memory_type        ON agent_memory(agent_id, memory_type);
CREATE INDEX IF NOT EXISTS idx_agent_memory_session     ON agent_memory(session_id) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_memory_expires     ON agent_memory(expires_at) WHERE expires_at IS NOT NULL;
-- Vector similarity index (IVFFlat for large-scale, HNSW for quality)
CREATE INDEX IF NOT EXISTS idx_agent_memory_embedding   ON agent_memory USING hnsw (embedding vector_cosine_ops)
    WHERE embedding IS NOT NULL;
DROP TRIGGER IF EXISTS set_agent_memory_updated_at ON agent_memory;
CREATE TRIGGER set_agent_memory_updated_at BEFORE UPDATE ON agent_memory FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
--  AGENT COMMUNICATION NETWORK (messaging bus)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS agent_messages (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    from_agent_id   UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    to_agent_id     UUID         REFERENCES agents(id) ON DELETE CASCADE,
    to_group        VARCHAR(255),  -- broadcast to a group of agents
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- message_type: task|result|request|response|broadcast|spawn
    message_type    VARCHAR(30)  NOT NULL DEFAULT 'task',
    subject         VARCHAR(500),
    payload         JSONB        NOT NULL DEFAULT '{}',
    correlation_id  UUID,   -- link request → response
    reply_to        UUID    REFERENCES agent_messages(id) ON DELETE SET NULL,
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    priority        INTEGER      NOT NULL DEFAULT 5,
    delivered_at    TIMESTAMPTZ,
    read_at         TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_messages_to      ON agent_messages(to_agent_id, status) WHERE to_agent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_messages_from    ON agent_messages(from_agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_messages_corr    ON agent_messages(correlation_id) WHERE correlation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_messages_status  ON agent_messages(status, created_at DESC);

-- Agent spawn tracking (parent → child relationships)
CREATE TABLE IF NOT EXISTS agent_spawn_tree (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    parent_agent_id UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    child_agent_id  UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    spawn_reason    TEXT,
    spawn_config    JSONB        NOT NULL DEFAULT '{}',
    status          VARCHAR(30)  NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (parent_agent_id, child_agent_id)
);
CREATE INDEX IF NOT EXISTS idx_spawn_tree_parent ON agent_spawn_tree(parent_agent_id);
CREATE INDEX IF NOT EXISTS idx_spawn_tree_child  ON agent_spawn_tree(child_agent_id);

-- ─────────────────────────────────────────────────────────────────────────────
--  AUTOSCALING POLICIES
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS autoscale_policies (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    enabled         BOOLEAN      NOT NULL DEFAULT TRUE,
    min_replicas    INTEGER      NOT NULL DEFAULT 1,
    max_replicas    INTEGER      NOT NULL DEFAULT 10,
    -- scale_metric: queue_depth | cpu_percent | memory_percent | latency_ms | rps
    scale_metric    VARCHAR(50)  NOT NULL DEFAULT 'queue_depth',
    scale_up_threshold   FLOAT  NOT NULL DEFAULT 80.0,
    scale_down_threshold FLOAT  NOT NULL DEFAULT 20.0,
    scale_up_cooldown_s  INTEGER NOT NULL DEFAULT 60,
    scale_down_cooldown_s INTEGER NOT NULL DEFAULT 300,
    scale_increment INTEGER      NOT NULL DEFAULT 1,
    current_replicas INTEGER     NOT NULL DEFAULT 1,
    last_scale_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_autoscale_agent   ON autoscale_policies(agent_id);
CREATE INDEX IF NOT EXISTS idx_autoscale_enabled ON autoscale_policies(enabled) WHERE enabled;
DROP TRIGGER IF EXISTS set_autoscale_updated_at ON autoscale_policies;
CREATE TRIGGER set_autoscale_updated_at BEFORE UPDATE ON autoscale_policies FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- Autoscale events log
CREATE TABLE IF NOT EXISTS autoscale_events (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    policy_id       UUID         NOT NULL REFERENCES autoscale_policies(id) ON DELETE CASCADE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    -- direction: up | down
    direction       VARCHAR(10)  NOT NULL,
    from_replicas   INTEGER      NOT NULL,
    to_replicas     INTEGER      NOT NULL,
    trigger_metric  VARCHAR(50)  NOT NULL,
    trigger_value   FLOAT        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_autoscale_events_policy ON autoscale_events(policy_id, created_at DESC);

-- ─────────────────────────────────────────────────────────────────────────────
--  NODE AUTOSCALING
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS node_scale_policies (
    id                  UUID    PRIMARY KEY DEFAULT uuid_generate_v4(),
    name                VARCHAR(255) NOT NULL,
    min_nodes           INTEGER NOT NULL DEFAULT 1,
    max_nodes           INTEGER NOT NULL DEFAULT 100,
    scale_up_cpu_pct    FLOAT   NOT NULL DEFAULT 80.0,
    scale_down_cpu_pct  FLOAT   NOT NULL DEFAULT 20.0,
    scale_up_cooldown_s INTEGER NOT NULL DEFAULT 120,
    scale_down_cooldown_s INTEGER NOT NULL DEFAULT 600,
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ─────────────────────────────────────────────────────────────────────────────
--  WEBHOOK TRIGGERS
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS webhooks (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    agent_id        UUID         REFERENCES agents(id) ON DELETE CASCADE,
    workflow_id     UUID         REFERENCES workflows(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    endpoint_path   VARCHAR(500) NOT NULL UNIQUE,
    secret_hash     VARCHAR(255),   -- HMAC-SHA256 of signing secret
    events          TEXT[]       NOT NULL DEFAULT '{}',
    enabled         BOOLEAN      NOT NULL DEFAULT TRUE,
    last_triggered  TIMESTAMPTZ,
    trigger_count   INTEGER      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_webhooks_org  ON webhooks(organisation_id);
CREATE INDEX IF NOT EXISTS idx_webhooks_path ON webhooks(endpoint_path);

-- ─────────────────────────────────────────────────────────────────────────────
--  SDK / CLI deployment records
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS deployments (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    version_id      UUID         REFERENCES agent_versions(id) ON DELETE SET NULL,
    -- strategy: rolling | blue_green | canary
    strategy        VARCHAR(30)  NOT NULL DEFAULT 'rolling',
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    config          JSONB        NOT NULL DEFAULT '{}',
    deployed_by     VARCHAR(255),
    rollback_to     UUID         REFERENCES agent_versions(id) ON DELETE SET NULL,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    error           TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_deployments_agent ON deployments(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_deployments_org   ON deployments(organisation_id);
DROP TRIGGER IF EXISTS set_deployments_updated_at ON deployments;
CREATE TRIGGER set_deployments_updated_at BEFORE UPDATE ON deployments FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
--  Verify
-- ─────────────────────────────────────────────────────────────────────────────
SELECT table_name FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name;

COMMIT;