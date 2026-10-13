-- Exemption channel for the native catalog checks. This file owns its shape and
-- its rows; scripts/quality/sql-lint.sql only reads it.
CREATE TEMP TABLE IF NOT EXISTS lint_ignore_object (
    identifier text PRIMARY KEY
);

-- Exemptions are exact three-part identifiers and need a scoped reason plus
-- maintainer review; the native naming checks keep inspecting everything else.
-- Add one exact identifier per line, for example:
-- INSERT INTO lint_ignore_object (identifier)
-- VALUES ('pin_glide.some_table.idx_legacy_name');
