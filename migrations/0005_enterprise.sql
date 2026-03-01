-- =============================================================================
--  AI Agent Infrastructure Platform
--  Migration: 0005_enterprise
--
--  Adds full enterprise capabilities:
--    - Multi-tenancy (organisation_members, org-scoped agents)
--    - Quota system (organisation_quotas + usage tracking)
--    - SSO (oauth_accounts, sessions, api_keys)
--    - Rate limiting support tables
--    - Kubernetes runtime columns
--
--  100% idempotent — safe to run on existing databases.
-- =============================================================================

BEGIN;

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ─────────────────────────────────────────────────────────────────────────────
--  Shared trigger
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trigger_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = NOW(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

-- =============================================================================
--  1. organisations  (extend existing table)
-- =============================================================================
-- Already exists from migration 0003. Backfill missing columns only.
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS display_name  VARCHAR(255);
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS description   TEXT;
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS logo_url      TEXT;
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS website       TEXT;
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS settings      JSONB NOT NULL DEFAULT '{}';
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS is_active     BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE organisations ADD COLUMN IF NOT EXISTS updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW();

DROP TRIGGER IF EXISTS set_organisations_updated_at ON organisations;
CREATE TRIGGER set_organisations_updated_at
  BEFORE UPDATE ON organisations
  FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();


-- =============================================================================
--  2. organisation_members  (many-to-many users ↔ organisations, with role)
--     A user can belong to multiple orgs with different roles per org.
-- =============================================================================
CREATE TABLE IF NOT EXISTS organisation_members (
    id              UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID        NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    user_id         UUID        NOT NULL REFERENCES users(id)         ON DELETE CASCADE,
    -- org-scoped role: owner | admin | developer | viewer
    role            VARCHAR(50) NOT NULL DEFAULT 'developer',
    invited_by      UUID        REFERENCES users(id) ON DELETE SET NULL,
    accepted_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (organisation_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_org_members_org    ON organisation_members(organisation_id);
CREATE INDEX IF NOT EXISTS idx_org_members_user   ON organisation_members(user_id);
CREATE INDEX IF NOT EXISTS idx_org_members_role   ON organisation_members(role);

DROP TRIGGER IF EXISTS set_org_members_updated_at ON organisation_members;
CREATE TRIGGER set_org_members_updated_at
  BEFORE UPDATE ON organisation_members
  FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();


-- =============================================================================
--  3. organisation_quotas  (hard limits per organisation)
-- =============================================================================
CREATE TABLE IF NOT EXISTS organisation_quotas (
    id                   UUID    PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id      UUID    NOT NULL UNIQUE REFERENCES organisations(id) ON DELETE CASCADE,

    -- Agent limits
    max_agents           INTEGER NOT NULL DEFAULT 10,
    max_running_agents   INTEGER NOT NULL DEFAULT 5,

    -- Compute limits (milliCPU and MiB)
    max_cpu_millicores   INTEGER NOT NULL DEFAULT 4000,    -- 4 vCPU total
    max_memory_mib       INTEGER NOT NULL DEFAULT 4096,    -- 4 GiB total

    -- Execution limits
    max_executions_day   INTEGER NOT NULL DEFAULT 1000,
    max_executions_month INTEGER NOT NULL DEFAULT 20000,
    max_concurrent_exec  INTEGER NOT NULL DEFAULT 20,

    -- Storage & logging
    max_log_retention_days INTEGER NOT NULL DEFAULT 30,
    max_log_gb           FLOAT   NOT NULL DEFAULT 10.0,

    -- API & scheduling
    max_api_rpm          INTEGER NOT NULL DEFAULT 1000,    -- requests per minute
    max_schedules        INTEGER NOT NULL DEFAULT 50,

    -- Kubernetes
    max_k8s_pods         INTEGER NOT NULL DEFAULT 0,       -- 0 = docker only

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DROP TRIGGER IF EXISTS set_org_quotas_updated_at ON organisation_quotas;
CREATE TRIGGER set_org_quotas_updated_at
  BEFORE UPDATE ON organisation_quotas
  FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();


-- =============================================================================
--  4. organisation_usage  (rolling usage counters — updated on every action)
--     Separate from quotas so quotas can be changed without touching usage.
-- =============================================================================
CREATE TABLE IF NOT EXISTS organisation_usage (
    id                   UUID    PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id      UUID    NOT NULL UNIQUE REFERENCES organisations(id) ON DELETE CASCADE,

    -- Live counters (updated in real time)
    current_agents       INTEGER NOT NULL DEFAULT 0,
    current_running      INTEGER NOT NULL DEFAULT 0,
    current_cpu_mc       INTEGER NOT NULL DEFAULT 0,   -- millicores in use
    current_memory_mib   INTEGER NOT NULL DEFAULT 0,
    current_exec         INTEGER NOT NULL DEFAULT 0,   -- active executions

    -- Rolling windows (reset by a cron job)
    executions_today     INTEGER NOT NULL DEFAULT 0,
    executions_month     INTEGER NOT NULL DEFAULT 0,
    api_requests_minute  INTEGER NOT NULL DEFAULT 0,   -- reset every minute

    -- Watermarks
    peak_agents          INTEGER NOT NULL DEFAULT 0,
    peak_running         INTEGER NOT NULL DEFAULT 0,

    period_start_day     DATE    NOT NULL DEFAULT CURRENT_DATE,
    period_start_month   DATE    NOT NULL DEFAULT DATE_TRUNC('month', CURRENT_DATE)::DATE,

    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

DROP TRIGGER IF EXISTS set_org_usage_updated_at ON organisation_usage;
CREATE TRIGGER set_org_usage_updated_at
  BEFORE UPDATE ON organisation_usage
  FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();

CREATE INDEX IF NOT EXISTS idx_org_usage_org ON organisation_usage(organisation_id);


-- =============================================================================
--  5. oauth_accounts  (SSO provider linkage)
--     One user can link multiple OAuth providers.
-- =============================================================================
CREATE TABLE IF NOT EXISTS oauth_accounts (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Provider: google | github | microsoft | okta | auth0 | generic_oidc
    provider        VARCHAR(50)  NOT NULL,
    provider_user_id VARCHAR(255) NOT NULL,   -- sub / id from provider
    email           VARCHAR(255),
    name            VARCHAR(255),
    avatar_url      TEXT,

    -- Token storage (encrypted at application layer)
    access_token    TEXT,
    refresh_token   TEXT,
    token_expires_at TIMESTAMPTZ,

    -- Provider-specific profile blob
    raw_profile     JSONB        NOT NULL DEFAULT '{}',

    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    UNIQUE (provider, provider_user_id)
);

CREATE INDEX IF NOT EXISTS idx_oauth_user_id   ON oauth_accounts(user_id);
CREATE INDEX IF NOT EXISTS idx_oauth_provider  ON oauth_accounts(provider, provider_user_id);

DROP TRIGGER IF EXISTS set_oauth_updated_at ON oauth_accounts;
CREATE TRIGGER set_oauth_updated_at
  BEFORE UPDATE ON oauth_accounts
  FOR EACH ROW EXECUTE FUNCTION trigger_set_updated_at();


-- =============================================================================
--  6. sessions  (JWT session tracking with refresh token rotation)
-- =============================================================================
CREATE TABLE IF NOT EXISTS sessions (
    id               UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id          UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organisation_id  UUID         REFERENCES organisations(id) ON DELETE SET NULL,

    -- Token ids (store hash, never plaintext JWT)
    access_jti       VARCHAR(255) NOT NULL UNIQUE,
    refresh_jti      VARCHAR(255) NOT NULL UNIQUE,
    refresh_token_hash VARCHAR(255) NOT NULL,  -- bcrypt hash of refresh token

    -- Validity
    access_expires_at  TIMESTAMPTZ NOT NULL,
    refresh_expires_at TIMESTAMPTZ NOT NULL,
    revoked            BOOLEAN     NOT NULL DEFAULT FALSE,
    revoked_at         TIMESTAMPTZ,
    revoke_reason      VARCHAR(100),

    -- Device / client fingerprint
    user_agent       TEXT,
    ip_address       INET,
    device_id        VARCHAR(255),

    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_used_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id         ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_access_jti      ON sessions(access_jti) WHERE NOT revoked;
CREATE INDEX IF NOT EXISTS idx_sessions_refresh_jti     ON sessions(refresh_jti) WHERE NOT revoked;
CREATE INDEX IF NOT EXISTS idx_sessions_refresh_expires ON sessions(refresh_expires_at) WHERE NOT revoked;
CREATE INDEX IF NOT EXISTS idx_sessions_org             ON sessions(organisation_id) WHERE organisation_id IS NOT NULL;


-- =============================================================================
--  7. api_keys  (long-lived machine-to-machine keys)
-- =============================================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id         UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organisation_id UUID         REFERENCES organisations(id) ON DELETE CASCADE,
    name            VARCHAR(255) NOT NULL,
    -- Store SHA-256 hash of the raw key; only shown once on creation
    key_hash        VARCHAR(255) NOT NULL UNIQUE,
    -- key_prefix: first 8 chars shown for identification (e.g. "pk_live_")
    key_prefix      VARCHAR(20)  NOT NULL,
    scopes          TEXT[]       NOT NULL DEFAULT '{}',  -- e.g. '{agents:read,agents:write}'
    expires_at      TIMESTAMPTZ,
    last_used_at    TIMESTAMPTZ,
    revoked         BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_api_keys_hash    ON api_keys(key_hash) WHERE NOT revoked;
CREATE INDEX IF NOT EXISTS idx_api_keys_user    ON api_keys(user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_org     ON api_keys(organisation_id) WHERE organisation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix  ON api_keys(key_prefix);


-- =============================================================================
--  8. rate_limit_counters  (persistent fallback for Redis rate limit state)
--     Redis is the primary store; this table is written only on Redis miss
--     and used for analytics / quota enforcement across restarts.
-- =============================================================================
CREATE TABLE IF NOT EXISTS rate_limit_counters (
    id            UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    key           VARCHAR(255) NOT NULL UNIQUE,  -- e.g. "org:<id>:rpm"
    count         INTEGER      NOT NULL DEFAULT 0,
    window_start  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    window_end    TIMESTAMPTZ  NOT NULL,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_rate_limit_key        ON rate_limit_counters(key);
CREATE INDEX IF NOT EXISTS idx_rate_limit_window_end ON rate_limit_counters(window_end);


-- =============================================================================
--  9. Kubernetes runtime columns  (backfill on agents + executions)
-- =============================================================================

-- Agents: support both docker and kubernetes runtimes
ALTER TABLE agents ADD COLUMN IF NOT EXISTS runtime         VARCHAR(20)  NOT NULL DEFAULT 'docker';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_namespace   VARCHAR(255) NOT NULL DEFAULT 'default';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_pod_name    VARCHAR(255);
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_job_name    VARCHAR(255);
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_labels      JSONB        NOT NULL DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_annotations JSONB        NOT NULL DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_node_selector JSONB      NOT NULL DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS k8s_tolerations JSONB        NOT NULL DEFAULT '[]';

-- Executions: Kubernetes-specific tracking
ALTER TABLE executions ADD COLUMN IF NOT EXISTS runtime       VARCHAR(20) NOT NULL DEFAULT 'docker';
ALTER TABLE executions ADD COLUMN IF NOT EXISTS k8s_pod_name  VARCHAR(255);
ALTER TABLE executions ADD COLUMN IF NOT EXISTS k8s_namespace VARCHAR(255);

-- Runtime enum check constraint (idempotent)
DO $$ BEGIN
  ALTER TABLE agents ADD CONSTRAINT agents_runtime_check
    CHECK (runtime IN ('docker','kubernetes'));
EXCEPTION WHEN duplicate_object THEN NULL; END $$;


-- =============================================================================
--  10. invitation_tokens  (org invitation flow)
-- =============================================================================
CREATE TABLE IF NOT EXISTS invitation_tokens (
    id              UUID         PRIMARY KEY DEFAULT uuid_generate_v4(),
    organisation_id UUID         NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
    invited_by      UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email           VARCHAR(255) NOT NULL,
    role            VARCHAR(50)  NOT NULL DEFAULT 'developer',
    token_hash      VARCHAR(255) NOT NULL UNIQUE,
    expires_at      TIMESTAMPTZ  NOT NULL,
    accepted_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_invitations_org   ON invitation_tokens(organisation_id);
CREATE INDEX IF NOT EXISTS idx_invitations_email ON invitation_tokens(email);
CREATE INDEX IF NOT EXISTS idx_invitations_token ON invitation_tokens(token_hash) WHERE accepted_at IS NULL;


-- =============================================================================
--  SEED: default quotas for existing organisations
-- =============================================================================
INSERT INTO organisation_quotas (organisation_id)
SELECT id FROM organisations
WHERE id NOT IN (SELECT organisation_id FROM organisation_quotas)
ON CONFLICT DO NOTHING;

INSERT INTO organisation_usage (organisation_id)
SELECT id FROM organisations
WHERE id NOT IN (SELECT organisation_id FROM organisation_usage)
ON CONFLICT DO NOTHING;


-- =============================================================================
--  Verification
-- =============================================================================
SELECT table_name,
       (SELECT COUNT(*) FROM information_schema.columns c
        WHERE c.table_name = t.table_name AND c.table_schema = 'public') AS columns,
       (SELECT COUNT(*) FROM pg_indexes i WHERE i.tablename = t.table_name AND i.schemaname = 'public') AS indexes
FROM information_schema.tables t
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name;

COMMIT;