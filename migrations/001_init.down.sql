DROP TABLE IF EXISTS ext_ecs_usage_state;
DROP TABLE IF EXISTS ext_ecs_usage_quotas;
DELETE FROM ext_ecs_usage_schema_migrations WHERE version = 1;
