-- Enable pg_stat_statements in every database.
-- Must run before schema migrations (00- prefix ensures ordering).
-- shared_preload_libraries = 'pg_stat_statements' must be set in postgresql.conf first.
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
