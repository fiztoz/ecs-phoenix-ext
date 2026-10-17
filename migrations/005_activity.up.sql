-- Derived write-side activity.
--
-- ECS namespace billing exposes no request counters, so "which bucket is
-- busy" cannot be read from the API: it is derived from the change in
-- objects/bytes/incomplete-multipart-parts between two metering
-- observations. Read traffic is invisible to this table by construction.
--
-- Rows are one hour of folded deltas, not one poll. That keeps the table
-- bounded (buckets x 24 rows per day at the default window) and makes
-- "busy in N of the last M hours" a small indexed scan. Only observations
-- where ECS metering actually advanced are folded in, so an hour with no
-- new metering data is absent rather than recorded as idle.

-- In-flight incomplete multipart parts, already returned by billing and
-- previously dropped at the poller layer. Non-zero means an upload was in
-- progress at observation time.
ALTER TABLE ext_ecs_usage_state ADD COLUMN mpu_parts BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS ext_ecs_usage_activity_hourly (
  namespace      VARCHAR(255) NOT NULL,
  bucket         VARCHAR(255) NOT NULL,
  -- DATETIME, not TIMESTAMP, on purpose: with MariaDB's default
  -- explicit_defaults_for_timestamp=OFF the first TIMESTAMP column in a table
  -- silently gains DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP.
  -- hour_bucket is a primary-key component and the additive upsert never
  -- re-assigns it, so that implicit annotation would rewrite the key on every
  -- fold and move counters into the wrong hour. DATETIME has no such
  -- behaviour and stores the UTC value the Go layer binds.
  hour_bucket    DATETIME NOT NULL,
  samples        INT NOT NULL DEFAULT 0,
  active_samples INT NOT NULL DEFAULT 0,
  objects_up     BIGINT NOT NULL DEFAULT 0,
  objects_down   BIGINT NOT NULL DEFAULT 0,
  bytes_up       BIGINT NOT NULL DEFAULT 0,
  bytes_down     BIGINT NOT NULL DEFAULT 0,
  mpu_parts_max  BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (namespace, bucket, hour_bucket)
);

-- Retention prune and the lookback window both filter on (namespace, hour).
CREATE INDEX IF NOT EXISTS ext_ecs_usage_activity_hourly_ns_hour
  ON ext_ecs_usage_activity_hourly (namespace, hour_bucket);
