CREATE TABLE IF NOT EXISTS ext_ecs_usage_schema_migrations (
  version     INT NOT NULL PRIMARY KEY,
  applied_at  TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS ext_ecs_usage_quotas (
  namespace    VARCHAR(255) NOT NULL,
  bucket       VARCHAR(255) NOT NULL,
  quota_bytes  BIGINT NOT NULL,
  updated_at   TIMESTAMP NOT NULL,
  PRIMARY KEY (namespace, bucket)
);

-- Durable last observation + hysteresis so a pod restart does not
-- forget a confirmed over-quota or the last good sample.
CREATE TABLE IF NOT EXISTS ext_ecs_usage_state (
  namespace       VARCHAR(255) NOT NULL,
  bucket          VARCHAR(255) NOT NULL,
  used_bytes      BIGINT NOT NULL,
  objects         BIGINT NOT NULL,
  mpu_bytes       BIGINT NOT NULL DEFAULT 0,
  sample_time     TIMESTAMP NULL,
  uptodate_till   TIMESTAMP NULL,
  polled_at       TIMESTAMP NOT NULL,
  over_streak     INT NOT NULL DEFAULT 0,
  confirmed_over  TINYINT(1) NOT NULL DEFAULT 0,
  last_error      TEXT NULL,
  PRIMARY KEY (namespace, bucket)
);
