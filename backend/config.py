from pydantic_settings import BaseSettings
from typing import Optional


class Settings(BaseSettings):
    APP_NAME: str = "AI Agent Infrastructure Platform"
    DEBUG: bool = False
    SECRET_KEY: str = "super-secret-key-change-in-production"
    
    DATABASE_URL: str = "postgresql+asyncpg://postgres:postgres@postgres:5432/agentdb"
    SYNC_DATABASE_URL: str = "postgresql://postgres:postgres@postgres:5432/agentdb"
    
    REDIS_URL: str = "redis://redis:6379"
    
    NATS_URL: str = "nats://nats:4222"
    
    EXECUTOR_URL: str = "http://executor:8081"
    METRICS_URL: str = "http://metrics-collector:8083"
    WS_GATEWAY_URL: str = "http://ws-gateway:8084"
    
    OTEL_EXPORTER_OTLP_ENDPOINT: str = "http://otel-collector:4317"
    PROMETHEUS_PORT: int = 8090
    
    DOCKER_SOCKET: str = "unix:///var/run/docker.sock"
    
    AGENT_IMAGE: str = "agent-runtime:latest"
    AGENT_NETWORK: str = "platform_network"

    class Config:
        env_file = ".env"


settings = Settings()