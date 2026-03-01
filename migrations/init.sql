\set ON_ERROR_STOP on

\echo 'running migration 0004_production_infrastructure.sql'
\i /docker-entrypoint-initdb.d/migrations/0004_production_infrastructure.sql

\echo 'running migration 0005_enterprise.sql'
\i /docker-entrypoint-initdb.d/migrations/0005_enterprise.sql

\echo 'running migration 0006_global_platform.sql'
\i /docker-entrypoint-initdb.d/migrations/0006_global_platform.sql

\echo 'running migration 0007_agent_runtime_extensions.sql'
\i /docker-entrypoint-initdb.d/migrations/0007_agent_runtime_extensions.sql

\echo 'running migration 0008_platform_extensions.sql'
\i /docker-entrypoint-initdb.d/migrations/0008_platform_extensions.sql

