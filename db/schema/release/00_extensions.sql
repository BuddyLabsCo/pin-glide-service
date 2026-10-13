CREATE SCHEMA IF NOT EXISTS release;
-- Schema Sync compares extensions against each target schema, so this
-- database-wide extension is declared in both desired schemas. The PostgreSQL
-- image ships and preloads it; job rows stay in Data Scripts.
CREATE EXTENSION IF NOT EXISTS pg_cron;
