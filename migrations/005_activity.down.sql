-- Dropping the table drops its index on both engines.
DROP TABLE IF EXISTS ext_ecs_usage_activity_hourly;
ALTER TABLE ext_ecs_usage_state DROP COLUMN mpu_parts;
DELETE FROM ext_ecs_usage_schema_migrations WHERE version = 5;
