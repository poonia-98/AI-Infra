from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    APP_NAME: str = "AI Agent Infrastructure Platform"
    VERSION: str = "3.0.0"
    DEBUG: bool = False

    # ── Security ──────────────────────────────────────────────────────────────
    SECRET_KEY: str = "super-secret-key-change-in-production"
    JWT_ALGORITHM: str = "HS256"
    ACCESS_TOKEN_EXPIRE_MINUTES: int = 60
    REFRESH_TOKEN_EXPIRE_DAYS: int = 30
    CONTROL_PLANE_API_KEY: str = ""
    ALLOWED_HOSTS: str = "*"
    CORS_ORIGINS: str = "http://localhost:3000,http://localhost:3001"
    MAX_REQUEST_BYTES: int = 10_485_760  # 10 MB

    # ── Database ──────────────────────────────────────────────────────────────
    DATABASE_URL: str = "postgresql+asyncpg://postgres:postgres@postgres:5432/agentdb"
    SYNC_DATABASE_URL: str = "postgresql://postgres:postgres@postgres:5432/agentdb"

    # ── Cache ─────────────────────────────────────────────────────────────────
    REDIS_URL: str = "redis://redis:6379"

    # ── Event Bus ─────────────────────────────────────────────────────────────
    NATS_URL: str = "nats://nats:4222"

    # ── Core Service URLs ─────────────────────────────────────────────────────
    EXECUTOR_URL: str = "http://executor:8081"
    K8S_EXECUTOR_URL: str = "http://k8s-executor:8091"
    METRICS_URL: str = "http://metrics-collector:8083"
    WS_GATEWAY_URL: str = "http://ws-gateway:8084"
    NODE_MANAGER_URL: str = "http://node-manager:8087"
    LOG_PROCESSOR_URL: str = "http://log-processor:8088"
    WORKFLOW_ENGINE_URL: str = "http://workflow-engine:8092"
    AGENT_AUTOSCALER_URL: str = "http://agent-autoscaler:8093"
    AGENT_GATEWAY_URL: str = "http://agent-gateway:8094"
    MARKETPLACE_URL: str = "http://marketplace-service:8095"

    # ── Cloud Platform Extension Service URLs (v3.0) ──────────────────────────
    BILLING_ENGINE_URL: str = "http://billing-engine:8101"
    SUBSCRIPTION_MGR_URL: str = "http://subscription-manager:8102"
    USAGE_METER_URL: str = "http://usage-meter:8100"
    SANDBOX_URL: str = "http://agent-sandbox-manager:8096"
    SIM_URL: str = "http://agent-simulation-engine:8097"
    GLOBAL_SCHED_URL: str = "http://global-scheduler:8098"
    REGION_CTRL_URL: str = "http://region-controller:8099"
    OBS_URL: str = "http://agent-observability:8109"
    VECTOR_URL: str = "http://memory-vector-engine:8103"
    SECRETS_MANAGER_URL: str = "http://secrets-manager:8104"
    MARKETPLACE_VALIDATOR_URL: str = "http://marketplace-validator:8105"
    AGENT_EVOLUTION_URL: str = "http://agent-evolution-engine:8106"
    AGENT_FEDERATION_URL: str = "http://agent-federation-network:8107"
    CLUSTER_CTRL_URL: str = "http://cluster-controller:8108"
    CONTROL_BRAIN_URL: str = "http://control-brain:8200"

    # ── Telemetry ─────────────────────────────────────────────────────────────
    OTEL_EXPORTER_OTLP_ENDPOINT: str = "http://otel-collector:4317"
    PROMETHEUS_PORT: int = 8090

    # ── Docker Runtime ────────────────────────────────────────────────────────
    DOCKER_SOCKET: str = "unix:///var/run/docker.sock"
    AGENT_IMAGE: str = "agent-runtime:latest"
    AGENT_NETWORK: str = "platform_network"

    # ── Kubernetes Runtime ────────────────────────────────────────────────────
    K8S_ENABLED: bool = False
    K8S_DEFAULT_NAMESPACE: str = "ai-agents"
    KUBECONFIG: str = ""

    # ── OAuth Providers ───────────────────────────────────────────────────────
    GOOGLE_CLIENT_ID: str = ""
    GOOGLE_CLIENT_SECRET: str = ""
    GITHUB_CLIENT_ID: str = ""
    GITHUB_CLIENT_SECRET: str = ""
    OIDC_DISCOVERY_URL: str = ""
    OIDC_CLIENT_ID: str = ""
    OIDC_CLIENT_SECRET: str = ""
    FRONTEND_URL: str = "http://localhost:3000"

    # ── Encryption ────────────────────────────────────────────────────────────
    SECRETS_PASSPHRASE: str = "change-me-in-production-SECRETS_PASSPHRASE"
    SECRETS_MASTER_KEY: str = "change-me-master-key-32bytes-min"

    # ── Rate Limiting ─────────────────────────────────────────────────────────
    RATE_LIMIT_ENABLED: bool = True
    RATE_LIMIT_IP_RPM: int = 60
    RATE_LIMIT_USER_RPM: int = 600
    RATE_LIMIT_ORG_RPM: int = 1000

    # ── Stripe Billing ────────────────────────────────────────────────────────
    STRIPE_SECRET_KEY: str = ""
    STRIPE_WEBHOOK_SECRET: str = ""

    # ── Vector / AI ───────────────────────────────────────────────────────────
    OPENAI_API_KEY: str = ""
    EMBEDDING_DIM: int = 1536

    class Config:
        env_file = ".env"
        case_sensitive = False


settings = Settings()

# Append after settings = Settings() — just re-read this carefully
# Actually this appends to bottom of file which already has settings = Settings()
# We need to fix this properly