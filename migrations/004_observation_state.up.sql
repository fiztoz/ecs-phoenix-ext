-- Cached ECS observation state, not operator-set quotas.
-- block_known/notify_known/default_known distinguish an explicit quota-off
-- (known=1, bytes NULL) from inventory that has never been observed
-- (known=0). A restart must not treat the latter as unlimited.

ALTER TABLE ext_ecs_usage_state ADD COLUMN block_bytes BIGINT NULL;
ALTER TABLE ext_ecs_usage_state ADD COLUMN block_known TINYINT(1) NOT NULL DEFAULT 0;
ALTER TABLE ext_ecs_usage_state ADD COLUMN notify_bytes BIGINT NULL;
ALTER TABLE ext_ecs_usage_state ADD COLUMN notify_known TINYINT(1) NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS ext_ecs_usage_namespace_state (
  namespace            VARCHAR(255) NOT NULL PRIMARY KEY,
  used_bytes           BIGINT NOT NULL,
  objects              BIGINT NOT NULL,
  polled_at            TIMESTAMP NOT NULL,
  over_streak          INT NOT NULL DEFAULT 0,
  confirmed_over       TINYINT(1) NOT NULL DEFAULT 0,
  default_block_bytes  BIGINT NULL,
  default_known        TINYINT(1) NOT NULL DEFAULT 0,
  block_bytes          BIGINT NULL,
  block_known          TINYINT(1) NOT NULL DEFAULT 0,
  notify_bytes         BIGINT NULL,
  notify_known         TINYINT(1) NOT NULL DEFAULT 0
);
