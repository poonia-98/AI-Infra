BEGIN;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "vector";

CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE IF NOT EXISTS agent_templates (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID REFERENCES organisations(id) ON DELETE SET NULL,
    name            VARCHAR(255) NOT NULL,
    slug            VARCHAR(255) NOT NULL UNIQUE,
    description     TEXT,
    category        VARCHAR(100),
    tags            TEXT[] NOT NULL DEFAULT '{}',
    agent_type      VARCHAR(100) NOT NULL DEFAULT 'langgraph',
    image           VARCHAR(500) NOT NULL,
    config_schema   JSONB NOT NULL DEFAULT '{}',
    default_config  JSONB NOT NULL DEFAULT '{}',
    default_env     JSONB NOT NULL DEFAULT '{}',
    version         VARCHAR(50) NOT NULL DEFAULT '1.0.0',
    is_public       BOOLEAN NOT NULL DEFAULT FALSE,
    is_verified     BOOLEAN NOT NULL DEFAULT FALSE,
    downloads       INTEGER NOT NULL DEFAULT 0,
    rating          FLOAT NOT NULL DEFAULT 0,
    rating_count    INTEGER NOT NULL DEFAULT 0,
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_templates_slug_0007 ON agent_templates(slug);
CREATE INDEX IF NOT EXISTS idx_agent_templates_category_0007 ON agent_templates(category);
CREATE INDEX IF NOT EXISTS idx_agent_templates_public_0007 ON agent_templates(is_public) WHERE is_public;
DROP TRIGGER IF EXISTS set_agent_templates_updated_at_0007 ON agent_templates;
CREATE TRIGGER set_agent_templates_updated_at_0007 BEFORE UPDATE ON agent_templates
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS agent_versions (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID REFERENCES organisations(id) ON DELETE SET NULL,
    version         VARCHAR(50) NOT NULL,
    image           VARCHAR(500) NOT NULL,
    config          JSONB NOT NULL DEFAULT '{}',
    env_vars        JSONB NOT NULL DEFAULT '{}',
    labels          JSONB NOT NULL DEFAULT '{}',
    is_current      BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (agent_id, version)
);
CREATE INDEX IF NOT EXISTS idx_agent_versions_agent_0007 ON agent_versions(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_versions_current_0007 ON agent_versions(agent_id, is_current) WHERE is_current;
DROP TRIGGER IF EXISTS set_agent_versions_updated_at_0007 ON agent_versions;
CREATE TRIGGER set_agent_versions_updated_at_0007 BEFORE UPDATE ON agent_versions
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS agent_memory (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID REFERENCES organisations(id) ON DELETE SET NULL,
    memory_type     VARCHAR(30) NOT NULL DEFAULT 'short_term',
    key             VARCHAR(500),
    content         TEXT NOT NULL,
    embedding       vector(1536),
    metadata        JSONB NOT NULL DEFAULT '{}',
    importance      FLOAT NOT NULL DEFAULT 0.5,
    access_count    INTEGER NOT NULL DEFAULT 0,
    last_accessed   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,
    session_id      VARCHAR(255),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_memory_agent_0007 ON agent_memory(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_memory_type_0007 ON agent_memory(agent_id, memory_type);
CREATE INDEX IF NOT EXISTS idx_agent_memory_session_0007 ON agent_memory(session_id) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_memory_expires_0007 ON agent_memory(expires_at) WHERE expires_at IS NOT NULL;
DROP TRIGGER IF EXISTS set_agent_memory_updated_at_0007 ON agent_memory;
CREATE TRIGGER set_agent_memory_updated_at_0007 BEFORE UPDATE ON agent_memory
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS execution_queues (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id  UUID REFERENCES organisations(id) ON DELETE CASCADE,
    agent_id         UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    workflow_exec_id UUID REFERENCES workflow_executions(id) ON DELETE SET NULL,
    queue_type       VARCHAR(30) NOT NULL DEFAULT 'standard',
    priority         INTEGER NOT NULL DEFAULT 5,
    status           VARCHAR(30) NOT NULL DEFAULT 'queued',
    input            JSONB NOT NULL DEFAULT '{}',
    assigned_node    VARCHAR(255),
    locked_by        VARCHAR(255),
    locked_at        TIMESTAMPTZ,
    lock_expires_at  TIMESTAMPTZ,
    attempt          INTEGER NOT NULL DEFAULT 1,
    max_attempts     INTEGER NOT NULL DEFAULT 3,
    last_error       TEXT,
    scheduled_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_execution_queues_status_0007 ON execution_queues(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_execution_queues_priority_0007 ON execution_queues(priority ASC, scheduled_at ASC) WHERE status='queued';
CREATE INDEX IF NOT EXISTS idx_execution_queues_agent_0007 ON execution_queues(agent_id);
DROP TRIGGER IF EXISTS set_execution_queues_updated_at_0007 ON execution_queues;
CREATE TRIGGER set_execution_queues_updated_at_0007 BEFORE UPDATE ON execution_queues
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS agent_communication (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    from_agent_id  UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    to_agent_id    UUID REFERENCES agents(id) ON DELETE CASCADE,
    channel        VARCHAR(255),
    message_type   VARCHAR(30) NOT NULL DEFAULT 'task',
    subject        VARCHAR(500),
    payload        JSONB NOT NULL DEFAULT '{}',
    status         VARCHAR(30) NOT NULL DEFAULT 'pending',
    priority       INTEGER NOT NULL DEFAULT 5,
    correlation_id UUID,
    expires_at     TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_communication_to_0007 ON agent_communication(to_agent_id, status);
CREATE INDEX IF NOT EXISTS idx_agent_communication_from_0007 ON agent_communication(from_agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_communication_corr_0007 ON agent_communication(correlation_id) WHERE correlation_id IS NOT NULL;
DROP TRIGGER IF EXISTS set_agent_communication_updated_at_0007 ON agent_communication;
CREATE TRIGGER set_agent_communication_updated_at_0007 BEFORE UPDATE ON agent_communication
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS agent_workflows (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id     UUID REFERENCES workflows(id) ON DELETE CASCADE,
    agent_id        UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID REFERENCES organisations(id) ON DELETE SET NULL,
    role            VARCHAR(50) NOT NULL DEFAULT 'worker',
    config          JSONB NOT NULL DEFAULT '{}',
    state           JSONB NOT NULL DEFAULT '{}',
    status          VARCHAR(30) NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_workflows_workflow_0007 ON agent_workflows(workflow_id);
CREATE INDEX IF NOT EXISTS idx_agent_workflows_agent_0007 ON agent_workflows(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_workflows_status_0007 ON agent_workflows(status);
DROP TRIGGER IF EXISTS set_agent_workflows_updated_at_0007 ON agent_workflows;
CREATE TRIGGER set_agent_workflows_updated_at_0007 BEFORE UPDATE ON agent_workflows
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

COMMIT;
