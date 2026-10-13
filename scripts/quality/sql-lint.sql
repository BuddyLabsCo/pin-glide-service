\set ON_ERROR_STOP on
BEGIN;

CREATE TEMP TABLE IF NOT EXISTS lint_ignore_object (
    identifier text PRIMARY KEY
) ON COMMIT DROP;

CREATE TEMP TABLE lint_violations (
    identifier text NOT NULL,
    violation text NOT NULL
) ON COMMIT DROP;

WITH foreign_keys AS (
    SELECT con.conname,
           n.nspname,
           source_table.relname AS source_table,
           target_table.relname AS target_table,
           string_agg(target_column.attname, '_' ORDER BY key.ord) AS target_columns
    FROM pg_constraint con
    JOIN pg_class source_table ON source_table.oid = con.conrelid
    JOIN pg_namespace n ON n.oid = source_table.relnamespace
    JOIN pg_class target_table ON target_table.oid = con.confrelid
    CROSS JOIN LATERAL unnest(con.conkey::int2[], con.confkey::int2[])
        WITH ORDINALITY AS key(source_attnum, target_attnum, ord)
    JOIN pg_attribute target_column
      ON target_column.attrelid = con.confrelid
     AND target_column.attnum = key.target_attnum
    WHERE n.nspname IN ('pin_glide', 'release')
      AND con.contype = 'f'
    GROUP BY con.conname, n.nspname, source_table.relname, target_table.relname
)
INSERT INTO lint_violations (identifier, violation)
SELECT format('%I.%I.%I', nspname, source_table, conname),
       format('foreign key must be named fk_%s_%s', target_table, target_columns)
FROM foreign_keys
WHERE conname <> 'fk_' || target_table || '_' || target_columns
  AND format('%I.%I.%I', nspname, source_table, conname)
      NOT IN (SELECT identifier FROM lint_ignore_object);

INSERT INTO lint_violations (identifier, violation)
SELECT format('%I.%I.%I', n.nspname, c.relname, con.conname),
       'primary key must be named pk_<table>'
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('pin_glide', 'release')
  AND con.contype = 'p'
  AND con.conname <> format('pk_%s', c.relname)
  AND format('%I.%I.%I', n.nspname, c.relname, con.conname)
      NOT IN (SELECT identifier FROM lint_ignore_object);

INSERT INTO lint_violations (identifier, violation)
SELECT format('%I.%I.%I', n.nspname, c.relname, con.conname),
       'unique constraints are not allowed; use a unique index'
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname IN ('pin_glide', 'release')
  AND con.contype = 'u'
  AND format('%I.%I.%I', n.nspname, c.relname, con.conname)
      NOT IN (SELECT identifier FROM lint_ignore_object);

WITH index_columns AS (
    SELECT i.indexrelid,
           n.nspname,
           t.relname AS table_name,
           idx.relname AS index_name,
           i.indisunique,
           bool_or(key.attnum = 0) AS has_expression,
           string_agg(a.attname, '_' ORDER BY key.ord) AS columns
    FROM pg_index i
    JOIN pg_class idx ON idx.oid = i.indexrelid
    JOIN pg_class t ON t.oid = i.indrelid
    JOIN pg_namespace n ON n.oid = t.relnamespace
    CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY
        AS key(attnum, ord)
    LEFT JOIN pg_attribute a
      ON a.attrelid = i.indrelid
     AND a.attnum = key.attnum
    WHERE n.nspname IN ('pin_glide', 'release')
      AND key.ord <= i.indnkeyatts
      AND NOT EXISTS (
          SELECT 1
          FROM pg_constraint con
          WHERE con.conindid = i.indexrelid
      )
    GROUP BY i.indexrelid, n.nspname, t.relname, idx.relname, i.indisunique
)
INSERT INTO lint_violations (identifier, violation)
SELECT format('%I.%I.%I', nspname, table_name, index_name),
       format('index must be named %s',
              CASE WHEN indisunique THEN 'uidx_' ELSE 'idx_' END
              || table_name || '_' || columns)
FROM index_columns
WHERE NOT has_expression
  AND index_name <> (CASE WHEN indisunique THEN 'uidx_' ELSE 'idx_' END
                     || table_name || '_' || columns)
  AND format('%I.%I.%I', nspname, table_name, index_name)
      NOT IN (SELECT identifier FROM lint_ignore_object);

TABLE lint_violations;
DO $$
DECLARE
    violation_count integer;
BEGIN
    SELECT count(*) INTO violation_count FROM lint_violations;
    IF violation_count > 0 THEN
        RAISE EXCEPTION 'SQL lint found % violation(s)', violation_count;
    END IF;
END $$;
COMMIT;
