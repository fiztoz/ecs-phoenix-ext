-- sample_time had no consumer: the UI shows the poll time, and ECS returns it
-- empty for most buckets (rendering as zero dates). Dropped end-to-end.
-- Plain DROP COLUMN (no IF EXISTS) so both MariaDB and SQLite accept it;
-- the migrations table guarantees it runs exactly once.
ALTER TABLE ext_ecs_usage_state DROP COLUMN sample_time;
