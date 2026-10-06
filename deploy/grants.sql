-- Least-privilege provisioning for ecs-phoenix-ext.
--
-- Run as an administrator. Substitute the account password from your secret
-- store for the REPLACE_BEFORE_RUN token before executing. Do not commit the
-- substituted file, and do not reuse Phoenix's application credentials.
--
-- The extension creates and uses tables only in the database selected by
-- DATABASE_DSN. Point that DSN at ecs_usage, not the Phoenix application
-- schema. Schema-level rights cover every table the process creates,
-- including tables added by later migrations — no per-table GRANT update is
-- required when a migration adds a table.
--
-- If a dedicated schema is impossible, do not grant every table in the Phoenix
-- application schema. Grant each
-- current extension table explicitly and repeat that GRANT before deploying
-- a migration that creates another table. Current tables:
--   ext_ecs_usage_schema_migrations
--   ext_ecs_usage_state
--   ext_ecs_usage_namespace_state
--   ext_ecs_usage_quotas
--   ext_ecs_usage_namespace_quotas

CREATE DATABASE IF NOT EXISTS ecs_usage CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;

CREATE USER IF NOT EXISTS 'ecs_usage'@'%' IDENTIFIED BY 'REPLACE_BEFORE_RUN';

GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX
  ON ecs_usage.* TO 'ecs_usage'@'%';

FLUSH PRIVILEGES;
