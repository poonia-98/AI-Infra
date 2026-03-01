-- =============================================================================
--  0008_complete_cloud_platform.sql
--  AI Agent Cloud Platform — Final enterprise tables
--  All remaining systems: billing, secrets vault, sandbox, observability,
--  multi-region, marketplace validation, simulation, vector memory,
--  federation, evolution engine
--  Idempotent — safe to re-run.
-- =============================================================================
BEGIN;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";  -- trigram for fast text search

CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$ BEGIN NEW.updated_at = NOW(); RETURN NEW; END; $$ LANGUAGE plpgsql;

-- =============================================================================
--  BILLING SYSTEM
-- =============================================================================

CREATE TABLE IF NOT EXISTS billing_plans (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    name            VARCHAR(100) NOT NULL UNIQUE,  -- free | starter | growth | enterprise
    display_name    VARCHAR(255) NOT NULL,
    description     TEXT,
    -- Pricing (in cents USD)
    base_price_cents        INTEGER NOT NULL DEFAULT 0,
    -- Per-unit pricing (in micro-cents for precision)
    price_per_agent_hour    BIGINT  NOT NULL DEFAULT 0,
    price_per_exec          BIGINT  NOT NULL DEFAULT 0,
    price_per_cpu_hour      BIGINT  NOT NULL DEFAULT 0,   -- per vCPU-hour
    price_per_gb_hour       BIGINT  NOT NULL DEFAULT 0,   -- per GB-hour memory
    price_per_storage_gb    BIGINT  NOT NULL DEFAULT 0,   -- per GB-month
    price_per_workflow_exec BIGINT  NOT NULL DEFAULT 0,
    price_per_api_call      BIGINT  NOT NULL DEFAULT 0,
    -- Included free tier (per billing period)
    free_agent_hours        BIGINT  NOT NULL DEFAULT 0,
    free_executions         BIGINT  NOT NULL DEFAULT 0,
    free_api_calls          BIGINT  NOT NULL DEFAULT 0,
    -- Limits (NULL = unlimited)
    max_agents              INTEGER,
    max_executions_month    INTEGER,
    max_workflow_executions INTEGER,
    max_api_calls_month     INTEGER,
    max_team_members        INTEGER,
    -- Features as JSON flags
    features        JSONB        NOT NULL DEFAULT '{}',
    is_active       BOOLEAN      NOT NULL DEFAULT TRUE,
    is_public       BOOLEAN      NOT NULL DEFAULT TRUE,
    sort_order      INTEGER      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_billing_plans_active ON billing_plans(is_active, sort_order);
DROP TRIGGER IF EXISTS set_billing_plans_updated_at ON billing_plans;
CREATE TRIGGER set_billing_plans_updated_at BEFORE UPDATE ON billing_plans
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS subscriptions (
    id                  UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id     UUID         NOT NULL UNIQUE REFERENCES organisations(id) ON DELETE CASCADE,
    plan_id             UUID         NOT NULL REFERENCES billing_plans(id),
    -- status: trialing | active | past_due | cancelled | expired
    status              VARCHAR(30)  NOT NULL DEFAULT 'active',
    -- Billing cycle: monthly | annual
    billing_cycle       VARCHAR(20)  NOT NULL DEFAULT 'monthly',
    current_period_start TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    current_period_end   TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '1 month'),
    trial_ends_at       TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    cancel_reason       TEXT,
    -- Payment method reference (stored in payment provider, never raw card data)
    payment_method_id   VARCHAR(255),
    payment_provider    VARCHAR(50)  NOT NULL DEFAULT 'stripe',
    external_id         VARCHAR(255),  -- stripe subscription id etc.
    metadata            JSONB        NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_subscriptions_org    ON subscriptions(organisation_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_plan   ON subscriptions(plan_id);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON subscriptions(status);
DROP TRIGGER IF EXISTS set_subscriptions_updated_at ON subscriptions;
CREATE TRIGGER set_subscriptions_updated_at BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS usage_records (
    id              UUID         NOT NULL DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    subscription_id UUID         REFERENCES subscriptions(id) ON DELETE SET NULL,
    -- resource_type: agent_hour | execution | cpu_hour | memory_gb_hour |
    --                storage_gb | workflow_execution | api_call | log_gb
    resource_type   VARCHAR(50)  NOT NULL,
    quantity        BIGINT       NOT NULL DEFAULT 1,
    unit            VARCHAR(20)  NOT NULL DEFAULT 'count',
    -- Cost in micro-cents (quantity * unit_price)
    unit_price_mc   BIGINT       NOT NULL DEFAULT 0,
    total_cost_mc   BIGINT       NOT NULL DEFAULT 0,
    -- Reference to the resource that generated this usage
    resource_id     UUID,
    resource_meta   JSONB        NOT NULL DEFAULT '{}',
    billed          BOOLEAN      NOT NULL DEFAULT FALSE,
    billed_at       TIMESTAMPTZ,
    invoice_id      UUID,
    period_start    TIMESTAMPTZ  NOT NULL DEFAULT DATE_TRUNC('hour', NOW()),
    period_end      TIMESTAMPTZ  NOT NULL DEFAULT DATE_TRUNC('hour', NOW()) + INTERVAL '1 hour',
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, recorded_at)
) PARTITION BY RANGE (recorded_at);

-- Create partitions for 2025-2026 (add more as needed)
DO $$ BEGIN
    FOR y IN 2025..2027 LOOP
        FOR m IN 1..12 LOOP
            DECLARE
                tname TEXT := format('usage_records_%s_%02d', y, m);
                start_date DATE := make_date(y, m, 1);
                end_date DATE := (make_date(y, m, 1) + INTERVAL '1 month')::DATE;
            BEGIN
                EXECUTE format(
                    'CREATE TABLE IF NOT EXISTS %I PARTITION OF usage_records '
                    'FOR VALUES FROM (%L) TO (%L)',
                    tname, start_date, end_date
                );
            END;
        END LOOP;
    END LOOP;
END $$;
CREATE TABLE IF NOT EXISTS usage_records_default PARTITION OF usage_records DEFAULT;

CREATE INDEX IF NOT EXISTS idx_usage_org_period    ON usage_records(organisation_id, period_start);
CREATE INDEX IF NOT EXISTS idx_usage_type          ON usage_records(resource_type, recorded_at);
CREATE INDEX IF NOT EXISTS idx_usage_unbilled       ON usage_records(organisation_id, billed) WHERE NOT billed;
CREATE INDEX IF NOT EXISTS idx_usage_invoice        ON usage_records(invoice_id) WHERE invoice_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS invoices (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    subscription_id UUID         REFERENCES subscriptions(id) ON DELETE SET NULL,
    invoice_number  VARCHAR(50)  NOT NULL UNIQUE,
    -- status: draft | open | paid | void | uncollectible
    status          VARCHAR(30)  NOT NULL DEFAULT 'draft',
    period_start    TIMESTAMPTZ  NOT NULL,
    period_end      TIMESTAMPTZ  NOT NULL,
    -- Amounts in cents
    subtotal_cents  BIGINT       NOT NULL DEFAULT 0,
    discount_cents  BIGINT       NOT NULL DEFAULT 0,
    tax_cents       BIGINT       NOT NULL DEFAULT 0,
    total_cents     BIGINT       NOT NULL DEFAULT 0,
    paid_cents      BIGINT       NOT NULL DEFAULT 0,
    line_items      JSONB        NOT NULL DEFAULT '[]',
    currency        VARCHAR(3)   NOT NULL DEFAULT 'USD',
    due_at          TIMESTAMPTZ,
    paid_at         TIMESTAMPTZ,
    voided_at       TIMESTAMPTZ,
    external_id     VARCHAR(255),
    pdf_url         TEXT,
    metadata        JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_invoices_org    ON invoices(organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_invoices_status ON invoices(status);
DROP TRIGGER IF EXISTS set_invoices_updated_at ON invoices;
CREATE TRIGGER set_invoices_updated_at BEFORE UPDATE ON invoices
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS payments (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    invoice_id      UUID         REFERENCES invoices(id) ON DELETE SET NULL,
    -- status: pending | succeeded | failed | refunded | disputed
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    amount_cents    BIGINT       NOT NULL,
    currency        VARCHAR(3)   NOT NULL DEFAULT 'USD',
    payment_method  VARCHAR(50)  NOT NULL DEFAULT 'card',
    provider        VARCHAR(50)  NOT NULL DEFAULT 'stripe',
    provider_txn_id VARCHAR(255),
    failure_code    VARCHAR(100),
    failure_message TEXT,
    refunded_cents  BIGINT       NOT NULL DEFAULT 0,
    metadata        JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_payments_org     ON payments(organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_payments_invoice ON payments(invoice_id);
DROP TRIGGER IF EXISTS set_payments_updated_at ON payments;
CREATE TRIGGER set_payments_updated_at BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS billing_credits (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    amount_cents    BIGINT       NOT NULL,
    reason          TEXT,
    expires_at      TIMESTAMPTZ,
    used_cents      BIGINT       NOT NULL DEFAULT 0,
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_credits_org ON billing_credits(organisation_id);

-- =============================================================================
--  SECRETS MANAGER (Distributed Vault)
-- =============================================================================

CREATE TABLE IF NOT EXISTS encryption_keys (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         REFERENCES organisations(id) ON DELETE CASCADE,
    key_id          VARCHAR(255) NOT NULL UNIQUE,  -- opaque ID like "key-orgid-v1"
    algorithm       VARCHAR(50)  NOT NULL DEFAULT 'AES-256-GCM',
    -- encrypted_key_material: key encrypted with master key (envelope encryption)
    encrypted_key   BYTEA        NOT NULL,
    -- status: active | rotating | retired
    status          VARCHAR(20)  NOT NULL DEFAULT 'active',
    rotated_from    UUID         REFERENCES encryption_keys(id) ON DELETE SET NULL,
    rotation_due_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    retired_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_enc_keys_org    ON encryption_keys(organisation_id);
CREATE INDEX IF NOT EXISTS idx_enc_keys_status ON encryption_keys(status);

CREATE TABLE IF NOT EXISTS vault_secrets (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    -- secret_type: generic | database | api_key | certificate | ssh_key | docker_registry
    secret_type     VARCHAR(50)  NOT NULL DEFAULT 'generic',
    name            VARCHAR(255) NOT NULL,
    path            VARCHAR(500) NOT NULL,  -- e.g. "/production/db/password"
    description     TEXT,
    -- Tags for filtering
    tags            JSONB        NOT NULL DEFAULT '{}',
    -- Rotation config
    auto_rotate     BOOLEAN      NOT NULL DEFAULT FALSE,
    rotate_days     INTEGER,
    next_rotation   TIMESTAMPTZ,
    last_rotated    TIMESTAMPTZ,
    -- Access control
    allowed_agents  UUID[]       NOT NULL DEFAULT '{}',  -- agent IDs with access
    allowed_envs    TEXT[]       NOT NULL DEFAULT '{"production","staging","development"}',
    requires_mfa    BOOLEAN      NOT NULL DEFAULT FALSE,
    -- Current version pointer
    current_version INTEGER      NOT NULL DEFAULT 1,
    status          VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (organisation_id, path)
);
CREATE INDEX IF NOT EXISTS idx_vault_secrets_org    ON vault_secrets(organisation_id);
CREATE INDEX IF NOT EXISTS idx_vault_secrets_path   ON vault_secrets(organisation_id, path);
CREATE INDEX IF NOT EXISTS idx_vault_secrets_rotate ON vault_secrets(auto_rotate, next_rotation) WHERE auto_rotate;
DROP TRIGGER IF EXISTS set_vault_secrets_updated_at ON vault_secrets;
CREATE TRIGGER set_vault_secrets_updated_at BEFORE UPDATE ON vault_secrets
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS secret_versions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    secret_id       UUID         NOT NULL REFERENCES vault_secrets(id) ON DELETE CASCADE,
    version         INTEGER      NOT NULL,
    -- encrypted_value: AES-256-GCM encrypted, nonce prepended
    encrypted_value BYTEA        NOT NULL,
    key_id          UUID         NOT NULL REFERENCES encryption_keys(id),
    checksum        VARCHAR(64)  NOT NULL,  -- SHA-256 of plaintext
    -- status: active | deprecated | destroyed
    status          VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    destroyed_at    TIMESTAMPTZ,
    UNIQUE (secret_id, version)
);
CREATE INDEX IF NOT EXISTS idx_secret_versions_secret ON secret_versions(secret_id, version DESC);
CREATE INDEX IF NOT EXISTS idx_secret_versions_status ON secret_versions(status);

CREATE TABLE IF NOT EXISTS secret_access_logs (
    id              UUID         NOT NULL DEFAULT uuid_generate_v4(),
    secret_id       UUID         NOT NULL REFERENCES vault_secrets(id) ON DELETE CASCADE,
    organisation_id UUID         NOT NULL,
    -- accessor_type: agent | user | service | api_key
    accessor_type   VARCHAR(30)  NOT NULL,
    accessor_id     VARCHAR(255) NOT NULL,
    -- action: read | write | rotate | delete | grant
    action          VARCHAR(30)  NOT NULL,
    secret_version  INTEGER,
    success         BOOLEAN      NOT NULL DEFAULT TRUE,
    error_code      VARCHAR(50),
    ip_address      INET,
    user_agent      TEXT,
    accessed_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, accessed_at)
) PARTITION BY RANGE (accessed_at);
CREATE TABLE IF NOT EXISTS secret_access_logs_2025 PARTITION OF secret_access_logs
    FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE IF NOT EXISTS secret_access_logs_2026 PARTITION OF secret_access_logs
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE IF NOT EXISTS secret_access_logs_2027 PARTITION OF secret_access_logs
    FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');
CREATE TABLE IF NOT EXISTS secret_access_logs_default PARTITION OF secret_access_logs DEFAULT;

CREATE INDEX IF NOT EXISTS idx_sal_secret     ON secret_access_logs(secret_id, accessed_at DESC);
CREATE INDEX IF NOT EXISTS idx_sal_accessor   ON secret_access_logs(accessor_id, accessed_at DESC);
CREATE INDEX IF NOT EXISTS idx_sal_org        ON secret_access_logs(organisation_id, accessed_at DESC);

-- =============================================================================
--  SANDBOX SECURITY
-- =============================================================================

CREATE TABLE IF NOT EXISTS sandbox_policies (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         REFERENCES organisations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    -- runtime: docker | gvisor | firecracker | kata
    runtime         VARCHAR(30)  NOT NULL DEFAULT 'docker',
    -- Network policy: none | restricted | full
    network_mode    VARCHAR(30)  NOT NULL DEFAULT 'restricted',
    allowed_hosts   TEXT[]       NOT NULL DEFAULT '{}',
    -- Filesystem
    readonly_root   BOOLEAN      NOT NULL DEFAULT TRUE,
    allowed_paths   TEXT[]       NOT NULL DEFAULT '{"/tmp","/var/run"}',
    blocked_paths   TEXT[]       NOT NULL DEFAULT '{"/proc/sysrq-trigger","/sys/kernel"}',
    -- Resources
    max_cpu_pct     FLOAT        NOT NULL DEFAULT 80.0,
    max_memory_mb   INTEGER      NOT NULL DEFAULT 512,
    max_pids        INTEGER      NOT NULL DEFAULT 256,
    max_open_files  INTEGER      NOT NULL DEFAULT 1024,
    -- Seccomp / capabilities
    seccomp_profile TEXT         NOT NULL DEFAULT 'default',
    dropped_caps    TEXT[]       NOT NULL DEFAULT '{"ALL"}',
    added_caps      TEXT[]       NOT NULL DEFAULT '{}',
    -- Syscall filtering
    blocked_syscalls TEXT[]      NOT NULL DEFAULT '{}',
    -- Time limits
    max_exec_seconds INTEGER     NOT NULL DEFAULT 3600,
    -- Policy as JSON for complex rules
    policy          JSONB        NOT NULL DEFAULT '{}',
    is_default      BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sandbox_policies_org     ON sandbox_policies(organisation_id);
CREATE INDEX IF NOT EXISTS idx_sandbox_policies_default ON sandbox_policies(is_default) WHERE is_default;
DROP TRIGGER IF EXISTS set_sandbox_policies_updated_at ON sandbox_policies;
CREATE TRIGGER set_sandbox_policies_updated_at BEFORE UPDATE ON sandbox_policies
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS sandbox_violations (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    policy_id       UUID         REFERENCES sandbox_policies(id) ON DELETE SET NULL,
    execution_id    UUID         REFERENCES executions(id) ON DELETE SET NULL,
    -- violation_type: syscall_blocked | network_blocked | fs_violation | resource_exceeded | capability_violation
    violation_type  VARCHAR(50)  NOT NULL,
    details         JSONB        NOT NULL DEFAULT '{}',
    severity        VARCHAR(20)  NOT NULL DEFAULT 'medium',  -- low | medium | high | critical
    action_taken    VARCHAR(50)  NOT NULL DEFAULT 'blocked',  -- blocked | killed | logged
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sandbox_violations_agent ON sandbox_violations(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sandbox_violations_org   ON sandbox_violations(organisation_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sandbox_violations_type  ON sandbox_violations(violation_type, severity);

-- =============================================================================
--  AGENT OBSERVABILITY
-- =============================================================================

CREATE TABLE IF NOT EXISTS execution_traces (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id    UUID         NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- OTEL trace/span correlation
    trace_id        VARCHAR(64)  NOT NULL,
    root_span_id    VARCHAR(32)  NOT NULL,
    -- Structured span tree stored as JSONB
    spans           JSONB        NOT NULL DEFAULT '[]',
    -- Performance breakdown (microseconds)
    total_duration_us   BIGINT,
    cpu_time_us         BIGINT,
    io_wait_us          BIGINT,
    network_wait_us     BIGINT,
    db_time_us          BIGINT,
    -- Resource usage samples
    peak_cpu_pct        FLOAT,
    peak_memory_mb      FLOAT,
    avg_cpu_pct         FLOAT,
    avg_memory_mb       FLOAT,
    -- Context
    tags            JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_exec_traces_execution ON execution_traces(execution_id);
CREATE INDEX IF NOT EXISTS idx_exec_traces_agent     ON execution_traces(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_exec_traces_trace_id  ON execution_traces(trace_id);

CREATE TABLE IF NOT EXISTS agent_lineage (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- Lineage chain: agent_id evolved from / cloned from / spawned from
    parent_agent_id UUID         REFERENCES agents(id) ON DELETE SET NULL,
    lineage_type    VARCHAR(30)  NOT NULL DEFAULT 'spawned',  -- spawned | cloned | evolved | forked
    template_id     UUID         REFERENCES agent_templates(id) ON DELETE SET NULL,
    version_id      UUID         REFERENCES agent_versions(id) ON DELETE SET NULL,
    depth           INTEGER      NOT NULL DEFAULT 0,  -- depth in lineage tree
    -- Full ancestry path as array for fast subtree queries
    ancestry        UUID[]       NOT NULL DEFAULT '{}',
    metadata        JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_lineage_agent   ON agent_lineage(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_lineage_parent  ON agent_lineage(parent_agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_lineage_ancestry ON agent_lineage USING GIN(ancestry);

CREATE TABLE IF NOT EXISTS performance_snapshots (
    id              UUID         NOT NULL DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- p50/p90/p99 latencies in ms
    p50_latency_ms  FLOAT,
    p90_latency_ms  FLOAT,
    p99_latency_ms  FLOAT,
    avg_latency_ms  FLOAT,
    -- Throughput
    executions_per_min FLOAT,
    success_rate    FLOAT,   -- 0-1
    error_rate      FLOAT,   -- 0-1
    -- Resource
    avg_cpu_pct     FLOAT,
    avg_memory_pct  FLOAT,
    -- Window
    window_start    TIMESTAMPTZ NOT NULL,
    window_end      TIMESTAMPTZ NOT NULL,
    sample_count    INTEGER     NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE TABLE IF NOT EXISTS performance_snapshots_2025 PARTITION OF performance_snapshots
    FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE IF NOT EXISTS performance_snapshots_2026 PARTITION OF performance_snapshots
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE TABLE IF NOT EXISTS performance_snapshots_default PARTITION OF performance_snapshots DEFAULT;

CREATE INDEX IF NOT EXISTS idx_perf_snapshots_agent  ON performance_snapshots(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_perf_snapshots_window ON performance_snapshots(window_start, window_end);

CREATE TABLE IF NOT EXISTS failure_diagnostics (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id    UUID         NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- failure_class: oom | timeout | crash | network | permission | unknown
    failure_class   VARCHAR(50)  NOT NULL DEFAULT 'unknown',
    error_code      VARCHAR(100),
    error_message   TEXT,
    stack_trace     TEXT,
    -- Diagnostic context
    context         JSONB        NOT NULL DEFAULT '{}',
    -- Suggested remediation
    remediation     JSONB        NOT NULL DEFAULT '[]',
    -- Was this automatically recovered?
    auto_recovered  BOOLEAN      NOT NULL DEFAULT FALSE,
    recovery_action VARCHAR(100),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_failure_diag_agent ON failure_diagnostics(agent_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_failure_diag_class ON failure_diagnostics(failure_class, created_at DESC);

-- =============================================================================
--  MULTI-REGION CONTROL PLANE
-- =============================================================================

CREATE TABLE IF NOT EXISTS regions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    name            VARCHAR(100) NOT NULL UNIQUE,  -- us-east-1, eu-west-1, ap-southeast-1
    display_name    VARCHAR(255) NOT NULL,
    provider        VARCHAR(50)  NOT NULL DEFAULT 'self-hosted',  -- aws | gcp | azure | self-hosted
    endpoint        TEXT,        -- API endpoint for this region's control plane
    api_key_hash    VARCHAR(255),  -- hashed API key for inter-region auth
    -- Geographic info
    latitude        FLOAT,
    longitude       FLOAT,
    country         VARCHAR(100),
    -- Status: active | degraded | offline | maintenance
    status          VARCHAR(30)  NOT NULL DEFAULT 'active',
    -- Capabilities
    supports_k8s    BOOLEAN      NOT NULL DEFAULT FALSE,
    supports_gpu    BOOLEAN      NOT NULL DEFAULT FALSE,
    max_nodes       INTEGER      NOT NULL DEFAULT 1000,
    -- Health
    last_heartbeat  TIMESTAMPTZ,
    latency_ms      INTEGER,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_regions_status ON regions(status);
DROP TRIGGER IF EXISTS set_regions_updated_at ON regions;
CREATE TRIGGER set_regions_updated_at BEFORE UPDATE ON regions
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS clusters (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    region_id       UUID         NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,  -- NULL = shared
    name            VARCHAR(255) NOT NULL,
    cluster_type    VARCHAR(30)  NOT NULL DEFAULT 'kubernetes',  -- kubernetes | docker-swarm | nomad
    endpoint        TEXT,
    kubeconfig      TEXT,        -- encrypted kubeconfig
    -- Resource capacity
    total_cpu_cores FLOAT        NOT NULL DEFAULT 0,
    total_memory_gb FLOAT        NOT NULL DEFAULT 0,
    total_nodes     INTEGER      NOT NULL DEFAULT 0,
    -- Current utilization
    used_cpu_cores  FLOAT        NOT NULL DEFAULT 0,
    used_memory_gb  FLOAT        NOT NULL DEFAULT 0,
    running_agents  INTEGER      NOT NULL DEFAULT 0,
    -- status: active | scaling | degraded | offline
    status          VARCHAR(30)  NOT NULL DEFAULT 'active',
    tags            JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_clusters_region ON clusters(region_id);
CREATE INDEX IF NOT EXISTS idx_clusters_status ON clusters(status);
DROP TRIGGER IF EXISTS set_clusters_updated_at ON clusters;
CREATE TRIGGER set_clusters_updated_at BEFORE UPDATE ON clusters
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS global_placements (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    region_id       UUID         NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    cluster_id      UUID         REFERENCES clusters(id) ON DELETE SET NULL,
    -- placement_type: primary | replica | canary
    placement_type  VARCHAR(30)  NOT NULL DEFAULT 'primary',
    priority        INTEGER      NOT NULL DEFAULT 1,
    -- Placement constraints
    constraints     JSONB        NOT NULL DEFAULT '{}',
    -- status: pending | scheduled | running | failed | migrating
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    scheduled_at    TIMESTAMPTZ,
    migrated_from   UUID         REFERENCES global_placements(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_placements_agent  ON global_placements(agent_id);
CREATE INDEX IF NOT EXISTS idx_placements_region ON global_placements(region_id, status);
DROP TRIGGER IF EXISTS set_placements_updated_at ON global_placements;
CREATE TRIGGER set_placements_updated_at BEFORE UPDATE ON global_placements
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS region_failover_events (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    source_region_id UUID        NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    target_region_id UUID        NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    trigger_reason  VARCHAR(100) NOT NULL,
    agents_migrated INTEGER      NOT NULL DEFAULT 0,
    duration_ms     BIGINT,
    -- status: in_progress | completed | failed | rolled_back
    status          VARCHAR(30)  NOT NULL DEFAULT 'in_progress',
    details         JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_failover_source ON region_failover_events(source_region_id, created_at DESC);

-- =============================================================================
--  MARKETPLACE VALIDATION
-- =============================================================================

CREATE TABLE IF NOT EXISTS template_scans (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    template_id     UUID         NOT NULL REFERENCES agent_templates(id) ON DELETE CASCADE,
    registry_id     UUID         REFERENCES agent_registry(id) ON DELETE SET NULL,
    -- scanner: trivy | snyk | clair | custom
    scanner         VARCHAR(50)  NOT NULL DEFAULT 'trivy',
    -- overall_result: pass | warn | fail | error
    overall_result  VARCHAR(20)  NOT NULL DEFAULT 'pending',
    -- Risk scores
    critical_count  INTEGER      NOT NULL DEFAULT 0,
    high_count      INTEGER      NOT NULL DEFAULT 0,
    medium_count    INTEGER      NOT NULL DEFAULT 0,
    low_count       INTEGER      NOT NULL DEFAULT 0,
    -- Full scan report
    scan_report     JSONB        NOT NULL DEFAULT '{}',
    -- Policy checks
    policy_checks   JSONB        NOT NULL DEFAULT '[]',
    malware_detected BOOLEAN     NOT NULL DEFAULT FALSE,
    scan_duration_ms BIGINT,
    image_digest    VARCHAR(255),
    scanned_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_template_scans_template ON template_scans(template_id, scanned_at DESC);
CREATE INDEX IF NOT EXISTS idx_template_scans_result   ON template_scans(overall_result);

CREATE TABLE IF NOT EXISTS template_approvals (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    template_id     UUID         NOT NULL REFERENCES agent_templates(id) ON DELETE CASCADE,
    scan_id         UUID         REFERENCES template_scans(id) ON DELETE SET NULL,
    -- status: pending | approved | rejected | revoked
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    reviewed_by     VARCHAR(255),
    review_notes    TEXT,
    -- approval_type: auto | manual
    approval_type   VARCHAR(20)  NOT NULL DEFAULT 'manual',
    conditions      JSONB        NOT NULL DEFAULT '[]',
    approved_at     TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_template_approvals_template ON template_approvals(template_id, status);

-- =============================================================================
--  AGENT SIMULATION
-- =============================================================================

CREATE TABLE IF NOT EXISTS simulation_environments (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    -- environment_type: isolated | shared | production-mirror
    environment_type VARCHAR(30) NOT NULL DEFAULT 'isolated',
    -- Synthetic workload config
    workload_config JSONB        NOT NULL DEFAULT '{}',
    -- Failure injection config
    fault_config    JSONB        NOT NULL DEFAULT '{}',
    -- Network conditions
    network_config  JSONB        NOT NULL DEFAULT '{}',
    -- status: active | paused | archived
    status          VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_by      VARCHAR(255),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sim_envs_org ON simulation_environments(organisation_id);
DROP TRIGGER IF EXISTS set_sim_envs_updated_at ON simulation_environments;
CREATE TRIGGER set_sim_envs_updated_at BEFORE UPDATE ON simulation_environments
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS simulation_runs (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    environment_id  UUID         NOT NULL REFERENCES simulation_environments(id) ON DELETE CASCADE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- run_type: behavior | performance | failure | regression | chaos
    run_type        VARCHAR(30)  NOT NULL DEFAULT 'behavior',
    config          JSONB        NOT NULL DEFAULT '{}',
    -- status: queued | running | completed | failed | cancelled
    status          VARCHAR(30)  NOT NULL DEFAULT 'queued',
    -- Results
    results         JSONB        NOT NULL DEFAULT '{}',
    assertions      JSONB        NOT NULL DEFAULT '[]',
    assertions_passed INTEGER    NOT NULL DEFAULT 0,
    assertions_total  INTEGER    NOT NULL DEFAULT 0,
    -- passed | failed | inconclusive
    verdict         VARCHAR(30),
    error           TEXT,
    duration_ms     BIGINT,
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sim_runs_env   ON simulation_runs(environment_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sim_runs_agent ON simulation_runs(agent_id, created_at DESC);

-- =============================================================================
--  VECTOR MEMORY ENGINE
-- =============================================================================

CREATE TABLE IF NOT EXISTS knowledge_bases (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    -- embedding_model: text-embedding-ada-002 | sentence-transformers | custom
    embedding_model VARCHAR(100) NOT NULL DEFAULT 'text-embedding-ada-002',
    embedding_dim   INTEGER      NOT NULL DEFAULT 1536,
    -- chunk strategy: fixed | sentence | paragraph | semantic
    chunk_strategy  VARCHAR(30)  NOT NULL DEFAULT 'paragraph',
    chunk_size      INTEGER      NOT NULL DEFAULT 512,
    chunk_overlap   INTEGER      NOT NULL DEFAULT 64,
    is_shared       BOOLEAN      NOT NULL DEFAULT FALSE,  -- shared across org agents
    total_chunks    INTEGER      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_knowledge_bases_org ON knowledge_bases(organisation_id);
DROP TRIGGER IF EXISTS set_kb_updated_at ON knowledge_bases;
CREATE TRIGGER set_kb_updated_at BEFORE UPDATE ON knowledge_bases
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS knowledge_chunks (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    knowledge_base_id UUID       NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    -- Source document tracking
    source_type     VARCHAR(50)  NOT NULL DEFAULT 'text',  -- text | file | url | agent_memory
    source_id       VARCHAR(500),
    source_url      TEXT,
    -- Content
    content         TEXT         NOT NULL,
    content_hash    VARCHAR(64)  NOT NULL,  -- SHA-256 for dedup
    -- Vector embedding (supports up to 3072-dim for text-embedding-3-large)
    embedding       vector(1536),
    -- Metadata
    metadata        JSONB        NOT NULL DEFAULT '{}',
    chunk_index     INTEGER      NOT NULL DEFAULT 0,
    token_count     INTEGER,
    -- status: indexed | stale | error
    status          VARCHAR(20)  NOT NULL DEFAULT 'indexed',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_kb      ON knowledge_chunks(knowledge_base_id);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_hash    ON knowledge_chunks(content_hash);
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_source  ON knowledge_chunks(source_id) WHERE source_id IS NOT NULL;
-- HNSW vector index for high-quality approximate nearest neighbor search
CREATE INDEX IF NOT EXISTS idx_knowledge_chunks_vector  ON knowledge_chunks
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64)
    WHERE embedding IS NOT NULL;

-- =============================================================================
--  AGENT FEDERATION NETWORK
-- =============================================================================

CREATE TABLE IF NOT EXISTS federation_peers (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- owner_org_id: organisation that owns this federation link
    owner_org_id    UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    peer_org_id     UUID         REFERENCES organisations(id) ON DELETE CASCADE,  -- NULL = external
    -- External federation
    peer_endpoint   TEXT,        -- REST/gRPC endpoint for the peer control plane
    peer_name       VARCHAR(255) NOT NULL,
    -- status: pending | active | suspended | revoked
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    -- Trust level: full | limited | readonly
    trust_level     VARCHAR(20)  NOT NULL DEFAULT 'limited',
    -- Capabilities shared
    capabilities    TEXT[]       NOT NULL DEFAULT '{"agent_messaging","task_dispatch"}',
    -- Signing key for request verification
    signing_key_hash VARCHAR(255),
    -- Rate limits for cross-federation traffic
    max_rps         INTEGER      NOT NULL DEFAULT 100,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_federation_peers_owner ON federation_peers(owner_org_id, status);
DROP TRIGGER IF EXISTS set_federation_peers_updated_at ON federation_peers;
CREATE TRIGGER set_federation_peers_updated_at BEFORE UPDATE ON federation_peers
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS federated_tasks (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    peer_id         UUID         NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    -- origin: outbound (we sent) | inbound (we received)
    origin          VARCHAR(20)  NOT NULL DEFAULT 'outbound',
    source_agent_id UUID         REFERENCES agents(id) ON DELETE SET NULL,
    -- Target can be in another org/cluster
    target_agent_id VARCHAR(255) NOT NULL,  -- remote agent ID (may not exist locally)
    target_endpoint TEXT,
    task_type       VARCHAR(50)  NOT NULL DEFAULT 'execute',
    payload         JSONB        NOT NULL DEFAULT '{}',
    -- status: pending | dispatched | running | completed | failed | timed_out
    status          VARCHAR(30)  NOT NULL DEFAULT 'pending',
    result          JSONB        NOT NULL DEFAULT '{}',
    error           TEXT,
    correlation_id  UUID,
    timeout_seconds INTEGER      NOT NULL DEFAULT 300,
    dispatched_at   TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_fed_tasks_peer   ON federated_tasks(peer_id, status);
CREATE INDEX IF NOT EXISTS idx_fed_tasks_corr   ON federated_tasks(correlation_id) WHERE correlation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_fed_tasks_source ON federated_tasks(source_agent_id) WHERE source_agent_id IS NOT NULL;

-- =============================================================================
--  AGENT EVOLUTION ENGINE
-- =============================================================================

CREATE TABLE IF NOT EXISTS agent_genomes (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    -- Genome version (increments on each evolution)
    genome_version  INTEGER      NOT NULL DEFAULT 1,
    -- Encoded traits: hyperparameters, config weights, behavior flags
    traits          JSONB        NOT NULL DEFAULT '{}',
    -- Performance fitness scores
    fitness_scores  JSONB        NOT NULL DEFAULT '{}',
    overall_fitness FLOAT        NOT NULL DEFAULT 0.0,  -- 0-1
    -- Lineage
    parent_genome_id UUID        REFERENCES agent_genomes(id) ON DELETE SET NULL,
    mutation_log    JSONB        NOT NULL DEFAULT '[]',  -- what changed from parent
    -- status: active | archived | experimental
    status          VARCHAR(20)  NOT NULL DEFAULT 'active',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (agent_id, genome_version)
);
CREATE INDEX IF NOT EXISTS idx_agent_genomes_agent   ON agent_genomes(agent_id, genome_version DESC);
CREATE INDEX IF NOT EXISTS idx_agent_genomes_fitness ON agent_genomes(overall_fitness DESC, created_at DESC);

CREATE TABLE IF NOT EXISTS evolution_experiments (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    -- strategy: genetic | gradient | random | bayesian
    strategy        VARCHAR(50)  NOT NULL DEFAULT 'genetic',
    base_agent_id   UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    -- Experiment config
    config          JSONB        NOT NULL DEFAULT '{}',
    -- Population management
    population_size INTEGER      NOT NULL DEFAULT 10,
    max_generations INTEGER      NOT NULL DEFAULT 20,
    current_generation INTEGER   NOT NULL DEFAULT 0,
    -- Fitness function definition
    fitness_function JSONB       NOT NULL DEFAULT '{}',
    selection_pressure FLOAT     NOT NULL DEFAULT 0.5,
    mutation_rate   FLOAT        NOT NULL DEFAULT 0.1,
    crossover_rate  FLOAT        NOT NULL DEFAULT 0.7,
    -- Best results
    best_agent_id   UUID         REFERENCES agents(id) ON DELETE SET NULL,
    best_fitness    FLOAT,
    -- status: running | paused | completed | failed
    status          VARCHAR(20)  NOT NULL DEFAULT 'running',
    started_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_evo_experiments_org  ON evolution_experiments(organisation_id);
CREATE INDEX IF NOT EXISTS idx_evo_experiments_base ON evolution_experiments(base_agent_id);
DROP TRIGGER IF EXISTS set_evo_experiments_updated_at ON evolution_experiments;
CREATE TRIGGER set_evo_experiments_updated_at BEFORE UPDATE ON evolution_experiments
    FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE TABLE IF NOT EXISTS cloned_agents (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    source_agent_id UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    clone_agent_id  UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    experiment_id   UUID         REFERENCES evolution_experiments(id) ON DELETE SET NULL,
    genome_id       UUID         REFERENCES agent_genomes(id) ON DELETE SET NULL,
    -- clone_type: exact | mutated | evolved
    clone_type      VARCHAR(20)  NOT NULL DEFAULT 'exact',
    mutations_applied JSONB      NOT NULL DEFAULT '[]',
    generation      INTEGER      NOT NULL DEFAULT 1,
    fitness_score   FLOAT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_cloned_agents_source ON cloned_agents(source_agent_id);
CREATE INDEX IF NOT EXISTS idx_cloned_agents_exp    ON cloned_agents(experiment_id);

-- =============================================================================
--  SEED DATA
-- =============================================================================

-- Default billing plans
INSERT INTO billing_plans (name, display_name, description, base_price_cents,
    price_per_agent_hour, price_per_exec, free_agent_hours, free_executions,
    max_agents, features, sort_order)
VALUES
    ('free', 'Free', 'Get started with AI agents', 0,
     0, 0, 100, 1000, 3,
     '{"workflows":false,"k8s":false,"custom_sandbox":false,"sso":false}', 1),
    ('starter', 'Starter', 'For growing teams', 2900,
     100, 10, 500, 10000, 25,
     '{"workflows":true,"k8s":false,"custom_sandbox":false,"sso":false}', 2),
    ('growth', 'Growth', 'Scale your agent infrastructure', 9900,
     80, 8, 2000, 50000, 100,
     '{"workflows":true,"k8s":true,"custom_sandbox":false,"sso":true}', 3),
    ('enterprise', 'Enterprise', 'Unlimited scale with dedicated support', 0,
     60, 6, 10000, 1000000, NULL,
     '{"workflows":true,"k8s":true,"custom_sandbox":true,"sso":true,"dedicated_support":true}', 4)
ON CONFLICT (name) DO NOTHING;

-- Default region (local/self-hosted)
INSERT INTO regions (name, display_name, provider, status, supports_k8s)
VALUES ('local', 'Local (Self-Hosted)', 'self-hosted', 'active', false)
ON CONFLICT (name) DO NOTHING;

-- Default sandbox policy
INSERT INTO sandbox_policies (name, description, runtime, network_mode, is_default)
VALUES ('default', 'Default sandbox policy', 'docker', 'restricted', true)
ON CONFLICT DO NOTHING;

COMMIT;