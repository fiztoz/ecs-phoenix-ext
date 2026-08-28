CREATE TABLE IF NOT EXISTS ext_ecs_usage_namespace_quotas (
  namespace    VARCHAR(255) NOT NULL PRIMARY KEY,
  quota_bytes  BIGINT NOT NULL,
  updated_at   TIMESTAMP NOT NULL
);
