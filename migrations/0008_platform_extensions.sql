BEGIN;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE execution_queues ADD COLUMN IF NOT EXISTS dedup_key VARCHAR(255);
ALTER TABLE execution_queues ADD COLUMN IF NOT EXISTS lease_id UUID;
ALTER TABLE execution_queues ADD COLUMN IF NOT EXISTS visibility_timeout_sec INTEGER NOT NULL DEFAULT 120;
ALTER TABLE execution_queues ADD COLUMN IF NOT EXISTS dead_letter_reason TEXT;
ALTER TABLE execution_queues ADD COLUMN IF NOT EXISTS lease_heartbeat_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_execution_queues_dedup ON execution_queues(dedup_key) WHERE dedup_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_execution_queues_lease_id ON execution_queues(lease_id) WHERE lease_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_execution_queues_lock_expire ON execution_queues(lock_expires_at) WHERE status IN ('leased','running');

CREATE TABLE IF NOT EXISTS billing_events (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID REFERENCES organisations(id) ON DELETE SET NULL,
    agent_id UUID REFERENCES agents(id) ON DELETE SET NULL,
    workflow_id UUID REFERENCES workflows(id) ON DELETE SET NULL,
    execution_id UUID REFERENCES executions(id) ON DELETE SET NULL,
    event_type VARCHAR(100) NOT NULL,
    quantity NUMERIC(20,6) NOT NULL DEFAULT 0,
    unit VARCHAR(50) NOT NULL DEFAULT 'unit',
    cost_usd NUMERIC(20,6) NOT NULL DEFAULT 0,
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_billing_events_org ON billing_events(organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_events_agent ON billing_events(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_billing_events_type ON billing_events(event_type, created_at DESC);
DROP TRIGGER IF EXISTS set_billing_events_updated_at ON billing_events;
CREATE TRIGGER set_billing_events_updated_at BEFORE UPDATE ON billing_events
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    webhook_id UUID NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event_type VARCHAR(100) NOT NULL,
    event_id UUID,
    target_url TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}',
    request_headers JSONB NOT NULL DEFAULT '{}',
    response_status INTEGER,
    response_body TEXT,
    attempt INTEGER NOT NULL DEFAULT 1,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    delivered BOOLEAN NOT NULL DEFAULT FALSE,
    next_attempt_at TIMESTAMPTZ,
    delivered_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_webhook ON webhook_deliveries(webhook_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_pending ON webhook_deliveries(delivered, next_attempt_at) WHERE delivered=FALSE;
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event ON webhook_deliveries(event_type, created_at DESC);
DROP TRIGGER IF EXISTS set_webhook_deliveries_updated_at ON webhook_deliveries;
CREATE TRIGGER set_webhook_deliveries_updated_at BEFORE UPDATE ON webhook_deliveries
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS agent_service_registry (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID REFERENCES organisations(id) ON DELETE SET NULL,
    endpoint VARCHAR(500) NOT NULL,
    protocol VARCHAR(50) NOT NULL DEFAULT 'nats',
    capabilities TEXT[] NOT NULL DEFAULT '{}',
    health_status VARCHAR(30) NOT NULL DEFAULT 'unknown',
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(agent_id)
);

CREATE INDEX IF NOT EXISTS idx_agent_service_registry_org ON agent_service_registry(organisation_id);
CREATE INDEX IF NOT EXISTS idx_agent_service_registry_health ON agent_service_registry(health_status, last_heartbeat DESC);
CREATE INDEX IF NOT EXISTS idx_agent_service_registry_caps ON agent_service_registry USING GIN(capabilities);
DROP TRIGGER IF EXISTS set_agent_service_registry_updated_at ON agent_service_registry;
CREATE TRIGGER set_agent_service_registry_updated_at BEFORE UPDATE ON agent_service_registry
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS governance_policies (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID REFERENCES organisations(id) ON DELETE CASCADE,
    name VARCHAR(255) NOT NULL,
    scope VARCHAR(50) NOT NULL DEFAULT 'global',
    policy_type VARCHAR(50) NOT NULL DEFAULT 'guardrail',
    rules JSONB NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by VARCHAR(255),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(organisation_id, name)
);

CREATE INDEX IF NOT EXISTS idx_governance_policies_org ON governance_policies(organisation_id);
CREATE INDEX IF NOT EXISTS idx_governance_policies_scope ON governance_policies(scope, enabled);
DROP TRIGGER IF EXISTS set_governance_policies_updated_at ON governance_policies;
CREATE TRIGGER set_governance_policies_updated_at BEFORE UPDATE ON governance_policies
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS approval_workflows (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID REFERENCES organisations(id) ON DELETE CASCADE,
    resource_type VARCHAR(100) NOT NULL,
    resource_id UUID,
    action VARCHAR(100) NOT NULL,
    requested_by VARCHAR(255),
    approval_chain JSONB NOT NULL DEFAULT '[]',
    status VARCHAR(30) NOT NULL DEFAULT 'pending',
    approved_by VARCHAR(255),
    approved_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_approval_workflows_org ON approval_workflows(organisation_id, status);
CREATE INDEX IF NOT EXISTS idx_approval_workflows_resource ON approval_workflows(resource_type, resource_id);
DROP TRIGGER IF EXISTS set_approval_workflows_updated_at ON approval_workflows;
CREATE TRIGGER set_approval_workflows_updated_at BEFORE UPDATE ON approval_workflows
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS federation_clusters (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name VARCHAR(255) NOT NULL UNIQUE,
    region VARCHAR(100) NOT NULL,
    endpoint VARCHAR(500) NOT NULL,
    api_key_hash VARCHAR(255),
    status VARCHAR(30) NOT NULL DEFAULT 'active',
    capacity INTEGER NOT NULL DEFAULT 0,
    used_capacity INTEGER NOT NULL DEFAULT 0,
    metadata JSONB NOT NULL DEFAULT '{}',
    last_sync_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_federation_clusters_region ON federation_clusters(region, status);
DROP TRIGGER IF EXISTS set_federation_clusters_updated_at ON federation_clusters;
CREATE TRIGGER set_federation_clusters_updated_at BEFORE UPDATE ON federation_clusters
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS federation_routes (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    cluster_id UUID NOT NULL REFERENCES federation_clusters(id) ON DELETE CASCADE,
    organisation_id UUID REFERENCES organisations(id) ON DELETE CASCADE,
    agent_id UUID REFERENCES agents(id) ON DELETE CASCADE,
    route_key VARCHAR(255) NOT NULL,
    target_endpoint VARCHAR(500) NOT NULL,
    weight INTEGER NOT NULL DEFAULT 100,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(cluster_id, route_key)
);

CREATE INDEX IF NOT EXISTS idx_federation_routes_org ON federation_routes(organisation_id, active);
CREATE INDEX IF NOT EXISTS idx_federation_routes_agent ON federation_routes(agent_id, active);
DROP TRIGGER IF EXISTS set_federation_routes_updated_at ON federation_routes;
CREATE TRIGGER set_federation_routes_updated_at BEFORE UPDATE ON federation_routes
FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

COMMIT;
