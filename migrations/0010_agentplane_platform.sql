-- =============================================================================
--  MIGRATION 0010 — AgentPlane Platform Completion  v3.0.0
--  Fully wires: Control Brain · Service Registry · Node Registry
--  Memory Engine · Federation · Time Travel · Platform State
--  Agent Versions · Decisions Log · Federation Trust · Embeddings
-- =============================================================================
BEGIN;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS "vector";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

-- =============================================================================
--  CONTROL BRAIN — SERVICE REGISTRY
-- =============================================================================

CREATE TABLE IF NOT EXISTS service_registry (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    service_name    VARCHAR(100) NOT NULL UNIQUE,
    service_url     VARCHAR(500) NOT NULL,
    health_endpoint VARCHAR(500),
    capabilities    TEXT[]       NOT NULL DEFAULT '{}',
    status          VARCHAR(20)  NOT NULL DEFAULT 'unknown',  -- healthy|degraded|down|unknown
    node            VARCHAR(100) NOT NULL DEFAULT 'local',
    version         VARCHAR(50),
    trust_level     VARCHAR(20)  NOT NULL DEFAULT 'internal', -- internal|external|federated
    metadata        JSONB        NOT NULL DEFAULT '{}',
    last_seen       TIMESTAMPTZ,
    registered_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_svc_reg_status   ON service_registry(status);
CREATE INDEX IF NOT EXISTS idx_svc_reg_lastseen ON service_registry(last_seen);

-- =============================================================================
--  CONTROL BRAIN — NODE REGISTRY
-- =============================================================================

CREATE TABLE IF NOT EXISTS node_registry (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    node_id         VARCHAR(100) NOT NULL UNIQUE,
    hostname        VARCHAR(255),
    region          VARCHAR(100) NOT NULL DEFAULT 'local',
    provider        VARCHAR(50)  NOT NULL DEFAULT 'docker',   -- docker|k8s|firecracker|gvisor
    cpu_cores       INTEGER      NOT NULL DEFAULT 0,
    memory_gb       DECIMAL(8,2) NOT NULL DEFAULT 0,
    disk_gb         DECIMAL(8,2) NOT NULL DEFAULT 0,
    cpu_percent     DECIMAL(5,2) NOT NULL DEFAULT 0,
    memory_percent  DECIMAL(5,2) NOT NULL DEFAULT 0,
    agent_count     INTEGER      NOT NULL DEFAULT 0,
    max_agents      INTEGER      NOT NULL DEFAULT 50,
    status          VARCHAR(20)  NOT NULL DEFAULT 'healthy',  -- healthy|degraded|draining|offline
    labels          JSONB        NOT NULL DEFAULT '{}',
    taints          JSONB        NOT NULL DEFAULT '[]',
    last_heartbeat  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_node_reg_region ON node_registry(region);
CREATE INDEX IF NOT EXISTS idx_node_reg_status ON node_registry(status);

-- =============================================================================
--  CONTROL BRAIN — DECISIONS LOG
-- =============================================================================

CREATE TABLE IF NOT EXISTS brain_decisions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    decision_type   VARCHAR(50)  NOT NULL,  -- scale_up|scale_down|failover|placement|restart
    agent_id        UUID         REFERENCES agents(id) ON DELETE SET NULL,
    service_name    VARCHAR(100),
    node_id         VARCHAR(100),
    reason          TEXT,
    action          JSONB        NOT NULL DEFAULT '{}',
    outcome         VARCHAR(20)  NOT NULL DEFAULT 'pending', -- pending|applied|failed|skipped
    outcome_detail  TEXT,
    executed_by     VARCHAR(50)  NOT NULL DEFAULT 'control-brain',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    executed_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_brain_decisions_type      ON brain_decisions(decision_type);
CREATE INDEX IF NOT EXISTS idx_brain_decisions_agent     ON brain_decisions(agent_id);
CREATE INDEX IF NOT EXISTS idx_brain_decisions_created   ON brain_decisions(created_at DESC);

-- =============================================================================
--  CONTROL BRAIN — PLATFORM STATE (global singleton-style KV store)
-- =============================================================================

CREATE TABLE IF NOT EXISTS platform_state (
    key             VARCHAR(200) PRIMARY KEY,
    value           JSONB        NOT NULL DEFAULT '{}',
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- =============================================================================
--  AGENT VERSIONS
-- =============================================================================

CREATE TABLE IF NOT EXISTS agent_versions (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    version_tag     VARCHAR(50)  NOT NULL,   -- e.g. v1.0, v1.1, evolved-gen5
    version_source  VARCHAR(30)  NOT NULL DEFAULT 'manual', -- manual|evolution|rollback|fork
    docker_image    VARCHAR(500),
    config_snapshot JSONB        NOT NULL DEFAULT '{}',
    prompt_snapshot TEXT,
    tool_list       TEXT[]       NOT NULL DEFAULT '{}',
    fitness_score   DECIMAL(6,4),
    change_summary  TEXT,
    is_active       BOOLEAN      NOT NULL DEFAULT FALSE,
    is_baseline     BOOLEAN      NOT NULL DEFAULT FALSE,
    promoted_from   UUID         REFERENCES agent_versions(id),
    created_by      UUID,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_versions_agent   ON agent_versions(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_versions_active  ON agent_versions(agent_id, is_active);

-- =============================================================================
--  MEMORY ENGINE — AGENT MEMORY
-- =============================================================================

CREATE TABLE IF NOT EXISTS agent_memory (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    execution_id    UUID         REFERENCES executions(id) ON DELETE SET NULL,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    session_id      VARCHAR(100),
    memory_type     VARCHAR(30)  NOT NULL DEFAULT 'episodic', -- episodic|semantic|procedural|working|long_term|short_term
    key             VARCHAR(255),
    content         TEXT         NOT NULL,
    content_summary TEXT,
    importance      DECIMAL(4,3) NOT NULL DEFAULT 0.5,  -- 0.0 - 1.0
    access_count    INTEGER      NOT NULL DEFAULT 0,
    last_accessed   TIMESTAMPTZ,
    ttl_seconds     INTEGER,
    expires_at      TIMESTAMPTZ,
    tags            TEXT[]       NOT NULL DEFAULT '{}',
    metadata        JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_memory_agent     ON agent_memory(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_memory_type      ON agent_memory(agent_id, memory_type);
CREATE INDEX IF NOT EXISTS idx_agent_memory_expires   ON agent_memory(expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_memory_session   ON agent_memory(session_id) WHERE session_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_agent_memory_importance ON agent_memory(importance DESC);

-- =============================================================================
--  MEMORY ENGINE — EMBEDDINGS (pgvector)
-- =============================================================================

CREATE TABLE IF NOT EXISTS memory_embeddings (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    memory_id       UUID         NOT NULL REFERENCES agent_memory(id) ON DELETE CASCADE UNIQUE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    embedding       vector(1536),           -- OpenAI ada-002 / text-embedding-3-small
    embedding_model VARCHAR(100) NOT NULL DEFAULT 'text-embedding-ada-002',
    embedding_dim   INTEGER      NOT NULL DEFAULT 1536,
    chunk_index     INTEGER      NOT NULL DEFAULT 0,
    chunk_text      TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_mem_embed_agent    ON memory_embeddings(agent_id);
CREATE INDEX IF NOT EXISTS idx_mem_embed_hnsw     ON memory_embeddings USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- =============================================================================
--  FEDERATION — PEERS
-- =============================================================================

CREATE TABLE IF NOT EXISTS federation_peers (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    peer_name       VARCHAR(255) NOT NULL,
    endpoint_url    VARCHAR(500) NOT NULL,
    public_key_hash VARCHAR(64),
    status          VARCHAR(20)  NOT NULL DEFAULT 'active',     -- active|inactive|banned|pending
    trust_level     VARCHAR(20)  NOT NULL DEFAULT 'sandboxed',  -- full|partial|sandboxed
    shared_secret   VARCHAR(500),  -- encrypted
    capabilities    TEXT[]       NOT NULL DEFAULT '{}',
    last_seen_at    TIMESTAMPTZ,
    ping_latency_ms INTEGER,
    tasks_sent      INTEGER      NOT NULL DEFAULT 0,
    tasks_received  INTEGER      NOT NULL DEFAULT 0,
    metadata        JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_fed_peers_status ON federation_peers(status);
CREATE INDEX IF NOT EXISTS idx_fed_peers_org    ON federation_peers(org_id);

-- =============================================================================
--  FEDERATION — TASKS
-- =============================================================================

CREATE TABLE IF NOT EXISTS federation_tasks (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    source_org_id    UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    target_peer_id   UUID         REFERENCES federation_peers(id) ON DELETE SET NULL,
    local_agent_id   UUID         REFERENCES agents(id) ON DELETE SET NULL,
    remote_agent_id  VARCHAR(100),
    task_type        VARCHAR(100) NOT NULL DEFAULT 'agent_delegation',
    payload          JSONB        NOT NULL DEFAULT '{}',
    result           JSONB        NOT NULL DEFAULT '{}',
    status           VARCHAR(20)  NOT NULL DEFAULT 'pending',  -- pending|dispatched|received|running|completed|failed
    trust_level      VARCHAR(20)  NOT NULL DEFAULT 'sandboxed',
    cost_budget_usd  DECIMAL(12,6) NOT NULL DEFAULT 0,
    actual_cost_usd  DECIMAL(12,6) NOT NULL DEFAULT 0,
    error_message    TEXT,
    expires_at       TIMESTAMPTZ,
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_fed_tasks_status     ON federation_tasks(status);
CREATE INDEX IF NOT EXISTS idx_fed_tasks_source_org ON federation_tasks(source_org_id);
CREATE INDEX IF NOT EXISTS idx_fed_tasks_peer       ON federation_tasks(target_peer_id);
CREATE INDEX IF NOT EXISTS idx_fed_tasks_agent      ON federation_tasks(local_agent_id);

-- =============================================================================
--  FEDERATION — TRUST POLICIES
-- =============================================================================

CREATE TABLE IF NOT EXISTS peer_trust_policies (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    peer_id         UUID         NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    policy_name     VARCHAR(100) NOT NULL,
    allowed_actions TEXT[]       NOT NULL DEFAULT '{}',
    max_cost_usd    DECIMAL(10,4) NOT NULL DEFAULT 10.0,
    rate_limit_rpm  INTEGER      NOT NULL DEFAULT 60,
    allowed_tools   TEXT[]       NOT NULL DEFAULT '{}',
    sandbox_runtime VARCHAR(20)  NOT NULL DEFAULT 'gvisor',  -- docker|gvisor|firecracker
    requires_approval BOOLEAN    NOT NULL DEFAULT FALSE,
    is_active       BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_trust_policies_peer ON peer_trust_policies(peer_id, is_active);

-- =============================================================================
--  TIME TRAVEL — EXECUTION STEPS
-- =============================================================================

CREATE TABLE IF NOT EXISTS execution_steps (
    id                UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id      UUID         NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
    agent_id          UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id   UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    step_number       INTEGER      NOT NULL,
    step_type         VARCHAR(50)  NOT NULL,  -- llm_call|tool_call|decision|memory_read|memory_write|http_call|output
    step_name         VARCHAR(255),
    -- I/O
    input_data        JSONB        NOT NULL DEFAULT '{}',
    output_data       JSONB        NOT NULL DEFAULT '{}',
    -- LLM fields
    prompt            TEXT,
    completion        TEXT,
    model             VARCHAR(100),
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    -- Tool fields
    tool_name         VARCHAR(100),
    tool_args         JSONB        NOT NULL DEFAULT '{}',
    tool_result       JSONB        NOT NULL DEFAULT '{}',
    -- Decision fields
    decision_type     VARCHAR(50),
    decision_reason   TEXT,
    alternatives      JSONB        NOT NULL DEFAULT '[]',
    -- Performance
    duration_ms       INTEGER,
    cost_usd          DECIMAL(12,8) NOT NULL DEFAULT 0,
    -- Status
    status            VARCHAR(20)  NOT NULL DEFAULT 'completed',  -- completed|failed|skipped
    error_message     TEXT,
    -- Replay support
    is_replayed       BOOLEAN      NOT NULL DEFAULT FALSE,
    parent_step_id    UUID         REFERENCES execution_steps(id),
    replay_of_step    UUID         REFERENCES execution_steps(id),
    metadata          JSONB        NOT NULL DEFAULT '{}',
    started_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_exec_steps_exec   ON execution_steps(execution_id);
CREATE INDEX IF NOT EXISTS idx_exec_steps_agent  ON execution_steps(agent_id);
CREATE INDEX IF NOT EXISTS idx_exec_steps_type   ON execution_steps(step_type);
CREATE INDEX IF NOT EXISTS idx_exec_steps_num    ON execution_steps(execution_id, step_number);

-- =============================================================================
--  TIME TRAVEL — EXECUTION TIMELINE (high-level view)
-- =============================================================================

CREATE TABLE IF NOT EXISTS execution_timelines (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    execution_id    UUID         NOT NULL REFERENCES executions(id) ON DELETE CASCADE UNIQUE,
    agent_id        UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    total_steps     INTEGER      NOT NULL DEFAULT 0,
    total_llm_calls INTEGER      NOT NULL DEFAULT 0,
    total_tool_calls INTEGER     NOT NULL DEFAULT 0,
    total_tokens    INTEGER      NOT NULL DEFAULT 0,
    total_cost_usd  DECIMAL(12,6) NOT NULL DEFAULT 0,
    peak_memory_mb  INTEGER,
    final_output    TEXT,
    error           TEXT,
    replay_count    INTEGER      NOT NULL DEFAULT 0,
    last_replay_at  TIMESTAMPTZ,
    metadata        JSONB        NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_exec_timeline_agent ON execution_timelines(agent_id);
CREATE INDEX IF NOT EXISTS idx_exec_timeline_exec  ON execution_timelines(execution_id);

-- =============================================================================
--  AGENT FITNESS (evolution engine)
-- =============================================================================

CREATE TABLE IF NOT EXISTS agent_fitness (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id         UUID         NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    experiment_id    UUID,
    genome_id        UUID,
    generation       INTEGER      NOT NULL DEFAULT 0,
    fitness_score    DECIMAL(8,6) NOT NULL DEFAULT 0,
    task_success_rate DECIMAL(5,4) NOT NULL DEFAULT 0,
    avg_latency_ms   DECIMAL(10,2) NOT NULL DEFAULT 0,
    avg_cost_usd     DECIMAL(12,8) NOT NULL DEFAULT 0,
    total_executions INTEGER      NOT NULL DEFAULT 0,
    successful_execs INTEGER      NOT NULL DEFAULT 0,
    evaluated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_agent_fitness_agent      ON agent_fitness(agent_id);
CREATE INDEX IF NOT EXISTS idx_agent_fitness_score      ON agent_fitness(fitness_score DESC);
CREATE INDEX IF NOT EXISTS idx_agent_fitness_experiment ON agent_fitness(experiment_id);

-- =============================================================================
--  SIMULATION RUNS (augments simulation_environments)
-- =============================================================================

CREATE TABLE IF NOT EXISTS simulation_runs (
    id                  UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    environment_id      UUID,
    organisation_id     UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    run_name            VARCHAR(255),
    scenario_type       VARCHAR(50)  NOT NULL DEFAULT 'load_test',
    status              VARCHAR(20)  NOT NULL DEFAULT 'queued', -- queued|running|completed|failed|cancelled
    workers             INTEGER      NOT NULL DEFAULT 1,
    target_agents       INTEGER,
    agents_launched     INTEGER      NOT NULL DEFAULT 0,
    agents_succeeded    INTEGER      NOT NULL DEFAULT 0,
    agents_failed       INTEGER      NOT NULL DEFAULT 0,
    peak_cpu_percent    DECIMAL(5,2),
    peak_memory_mb      INTEGER,
    p50_duration_ms     INTEGER,
    p95_duration_ms     INTEGER,
    p99_duration_ms     INTEGER,
    total_cost_usd      DECIMAL(12,6) NOT NULL DEFAULT 0,
    error_rate          DECIMAL(5,4)  NOT NULL DEFAULT 0,
    results             JSONB        NOT NULL DEFAULT '{}',
    error_message       TEXT,
    seed                INTEGER,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    duration_seconds    INTEGER,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_sim_runs_env    ON simulation_runs(environment_id);
CREATE INDEX IF NOT EXISTS idx_sim_runs_status ON simulation_runs(status);
CREATE INDEX IF NOT EXISTS idx_sim_runs_org    ON simulation_runs(organisation_id);

-- =============================================================================
--  REGION FAILOVER EVENTS
-- =============================================================================

CREATE TABLE IF NOT EXISTS region_failover_events (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    from_region_id   UUID         REFERENCES deployment_regions(id) ON DELETE SET NULL,
    to_region_id     UUID         REFERENCES deployment_regions(id) ON DELETE SET NULL,
    trigger_type     VARCHAR(50)  NOT NULL DEFAULT 'automatic', -- automatic|manual|scheduled
    reason           VARCHAR(255),
    agents_migrated  INTEGER      NOT NULL DEFAULT 0,
    migration_time_s INTEGER,
    status           VARCHAR(20)  NOT NULL DEFAULT 'completed',  -- initiated|migrating|completed|failed
    error_message    TEXT,
    initiated_by     VARCHAR(100),
    metadata         JSONB        NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_failover_from   ON region_failover_events(from_region_id);
CREATE INDEX IF NOT EXISTS idx_failover_status ON region_failover_events(status);
CREATE INDEX IF NOT EXISTS idx_failover_time   ON region_failover_events(created_at DESC);

-- =============================================================================
--  MARKETPLACE — AGENT TASKS
-- =============================================================================

CREATE TABLE IF NOT EXISTS agent_marketplace_tasks (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    buyer_agent_id  UUID         REFERENCES agents(id) ON DELETE SET NULL,
    seller_agent_id UUID         REFERENCES agents(id) ON DELETE SET NULL,
    buyer_org_id    UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    seller_org_id   UUID         REFERENCES organisations(id) ON DELETE SET NULL,
    task_type       VARCHAR(100) NOT NULL,
    task_input      JSONB        NOT NULL DEFAULT '{}',
    task_output     JSONB        NOT NULL DEFAULT '{}',
    status          VARCHAR(20)  NOT NULL DEFAULT 'pending',  -- pending|accepted|running|completed|failed|cancelled
    price_usd       DECIMAL(10,6) NOT NULL DEFAULT 0,
    escrow_held     BOOLEAN      NOT NULL DEFAULT FALSE,
    settled_at      TIMESTAMPTZ,
    error_message   TEXT,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_mkt_tasks_buyer  ON agent_marketplace_tasks(buyer_agent_id);
CREATE INDEX IF NOT EXISTS idx_mkt_tasks_seller ON agent_marketplace_tasks(seller_agent_id);
CREATE INDEX IF NOT EXISTS idx_mkt_tasks_status ON agent_marketplace_tasks(status);

-- =============================================================================
--  AUDIT LOG (enhanced — add brain decisions link)
-- =============================================================================

ALTER TABLE audit_logs
    ADD COLUMN IF NOT EXISTS brain_decision_id UUID REFERENCES brain_decisions(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS service_name VARCHAR(100),
    ADD COLUMN IF NOT EXISTS duration_ms INTEGER;

-- =============================================================================
--  SEED: Service Registry — register all known microservices
-- =============================================================================

INSERT INTO service_registry (service_name, service_url, health_endpoint, trust_level, capabilities, status) VALUES
    ('backend',                'http://backend:8000',                'http://backend:8000/health',                'internal', '{"agents","executions","billing","auth","memory","federation"}',  'unknown'),
    ('executor',               'http://executor:8081',               'http://executor:8081/health',               'internal', '{"docker","execution","containers"}',                             'unknown'),
    ('event-processor',        'http://event-processor:8082',        'http://event-processor:8082/health',        'internal', '{"events","nats","fanout"}',                                      'unknown'),
    ('metrics-collector',      'http://metrics-collector:8083',      'http://metrics-collector:8083/health',      'internal', '{"metrics","prometheus","containers"}',                           'unknown'),
    ('websocket-gateway',      'http://websocket-gateway:8084',      'http://websocket-gateway:8084/health',      'internal', '{"websocket","realtime","push"}',                                 'unknown'),
    ('reconciliation-service', 'http://reconciliation-service:8085', 'http://reconciliation-service:8085/health', 'internal', '{"reconcile","drift","selfheal"}',                                'unknown'),
    ('scheduler-service',      'http://scheduler-service:8086',      'http://scheduler-service:8086/health',      'internal', '{"scheduling","cron","triggers"}',                                'unknown'),
    ('node-manager',           'http://node-manager:8087',           'http://node-manager:8087/health',           'internal', '{"nodes","capacity","heartbeat"}',                                'unknown'),
    ('log-processor',          'http://log-processor:8088',          'http://log-processor:8088/health',          'internal', '{"logs","ingestion","batching"}',                                 'unknown'),
    ('workflow-engine',        'http://workflow-engine:8092',        'http://workflow-engine:8092/health',        'internal', '{"workflows","dag","orchestration"}',                             'unknown'),
    ('agent-autoscaler',       'http://agent-autoscaler:8093',       'http://agent-autoscaler:8093/health',       'internal', '{"autoscaling","replicas","demand"}',                             'unknown'),
    ('agent-gateway',          'http://agent-gateway:8094',          'http://agent-gateway:8094/health',          'internal', '{"routing","agent2agent","proxy"}',                               'unknown'),
    ('marketplace-service',    'http://marketplace-service:8095',    'http://marketplace-service:8095/health',    'internal', '{"marketplace","listings","search"}',                             'unknown'),
    ('agent-sandbox-manager',  'http://agent-sandbox-manager:8096',  'http://agent-sandbox-manager:8096/health',  'internal', '{"sandbox","isolation","policy","gvisor","firecracker"}',         'unknown'),
    ('agent-simulation-engine','http://agent-simulation-engine:8097','http://agent-simulation-engine:8097/health','internal', '{"simulation","chaos","loadtest","synthetic"}',                   'unknown'),
    ('global-scheduler',       'http://global-scheduler:8098',       'http://global-scheduler:8098/health',       'internal', '{"placement","multiregion","scheduling"}',                        'unknown'),
    ('region-controller',      'http://region-controller:8099',      'http://region-controller:8099/health',      'internal', '{"regions","failover","placement"}',                              'unknown'),
    ('usage-meter',            'http://usage-meter:8100',            'http://usage-meter:8100/health',            'internal', '{"metering","usage","billing"}',                                  'unknown'),
    ('billing-engine',         'http://billing-engine:8101',         'http://billing-engine:8101/health',         'internal', '{"billing","invoices","stripe","plans"}',                         'unknown'),
    ('subscription-manager',   'http://subscription-manager:8102',   'http://subscription-manager:8102/health',   'internal', '{"subscriptions","plans","quotas"}',                              'unknown'),
    ('memory-vector-engine',   'http://memory-vector-engine:8103',   'http://memory-vector-engine:8103/health',   'internal', '{"memory","embeddings","pgvector","search"}',                     'unknown'),
    ('secrets-manager',        'http://secrets-manager:8104',        'http://secrets-manager:8104/health',        'internal', '{"vault","aes256","secrets","rotation"}',                         'unknown'),
    ('marketplace-validator',  'http://marketplace-validator:8105',  'http://marketplace-validator:8105/health',  'internal', '{"validation","security","publish"}',                             'unknown'),
    ('agent-evolution-engine', 'http://agent-evolution-engine:8106', 'http://agent-evolution-engine:8106/health', 'internal', '{"evolution","genetics","fitness","optimization"}',               'unknown'),
    ('agent-federation-network','http://agent-federation-network:8107','http://agent-federation-network:8107/health','internal','{"federation","peers","trust","cross-org"}',                   'unknown'),
    ('cluster-controller',     'http://cluster-controller:8108',     'http://cluster-controller:8108/health',     'internal', '{"clusters","nodes","k8s"}',                                      'unknown'),
    ('agent-observability',    'http://agent-observability:8109',    'http://agent-observability:8109/health',    'internal', '{"traces","lineage","performance","diagnostics"}',                'unknown'),
    ('control-brain',          'http://control-brain:8200',          'http://control-brain:8200/health',          'internal', '{"orchestration","state","reconcile","placement","scale"}',       'unknown')
ON CONFLICT (service_name) DO UPDATE
    SET service_url     = EXCLUDED.service_url,
        capabilities    = EXCLUDED.capabilities,
        updated_at      = NOW();

-- =============================================================================
--  SEED: Platform State
-- =============================================================================

INSERT INTO platform_state (key, value) VALUES
    ('platform.version',   '"3.0.0"'),
    ('platform.name',      '"AgentPlane"'),
    ('platform.installed', 'true'),
    ('platform.features',  '{"control_brain":true,"vector_memory":true,"federation":true,"time_travel":true,"evolution":true,"simulation":true}')
ON CONFLICT (key) DO NOTHING;

COMMIT;