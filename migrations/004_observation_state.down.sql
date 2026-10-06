DROP TABLE IF EXISTS ext_ecs_usage_namespace_state;
ALTER TABLE ext_ecs_usage_state DROP COLUMN notify_known;
ALTER TABLE ext_ecs_usage_state DROP COLUMN notify_bytes;
ALTER TABLE ext_ecs_usage_state DROP COLUMN block_known;
ALTER TABLE ext_ecs_usage_state DROP COLUMN block_bytes;
DELETE FROM ext_ecs_usage_schema_migrations WHERE version = 4;
