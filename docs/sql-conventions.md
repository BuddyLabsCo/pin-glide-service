# Pin Glide SQL conventions

The desired schemas under `db/schema/pin_glide` and `db/schema/release` are linted
by `make lint-sql`. The pipeline applies each schema to a disposable PostgreSQL
database, runs native catalog checks, runs Schemalint, and then runs SQLFluff.
It never changes a shared database.

## Object names

All application objects are in `pin_glide`; release metadata objects are in `release`.

| Object | Required name |
| --- | --- |
| Table | `<domain>_<plural_noun>` |
| View | `<domain>_vw_<noun>` |
| Enum | `<domain>_<noun>` |
| User function | `<domain>_fn_<description>` |
| Stored procedure | `<domain>_sp_<description>` |
| Primary key | `pk_<table>` |
| Foreign key | `fk_<referenced_table>_<referenced_column>`; join composite columns in referenced-column order |
| Index | `idx_<table>_<columns>` |
| Unique index | `uidx_<table>_<columns>` |
| Check constraint | `chk_<description>` |
| Trigger | `trg_<table>_<description>` |

Names are lowercase snake case. Index column names are joined in index key
order. INCLUDE columns are not part of an index name. Expression indexes are
not checked by the native naming rule. Unique constraints are not allowed;
use a unique index instead. Default expressions remain inline and do not need a
constraint name.

The native checks inspect objects in `pin_glide` and `release`; extension-owned
cron objects are excluded.
Composite foreign-key order is checked from PostgreSQL catalog ordinality because
Schemalint does not expose `conkey` order.
Native violations can be exempted with an exact three-part identifier in
`scripts/quality/sql-lint-ignore.sql`:

```sql
INSERT INTO lint_ignore_object (identifier)
VALUES ('pin_glide.some_table.idx_legacy_name');
```

Schemalint violations use its built-in `ignores` facility when exceptions are
needed.

Both mechanisms are scoped exemptions, not blanket disables: the exemption names
the exact object, states why the rule cannot be followed, and needs maintainer
review. The catalog, Schemalint and SQLFluff checks cover names, comments and
formatting; they do not prove object ownership, migration safety or business
behavior.

## Comments

Every table, view, enum, user function, and stored procedure has a non-empty
`COMMENT ON` statement. Every table column has a non-empty `COMMENT ON COLUMN`.

A function or procedure with parameters includes a `parameters:` line in its
comment and one non-empty `<parameter_name>:` line for every PostgreSQL
parameter name. Parameter names are written without `@` and may appear in any
order.

## Formatting

SQLFluff uses PostgreSQL syntax, the raw templater, an 80-character line limit,
four-space indentation, uppercase keywords, lowercase types, and trailing
comma layout. Identifier capitalization (`CP02`) is excluded because the
naming rules above own identifier spelling.

SQLFluff also checks `db/migrations/pin_glide`, `db/migrations/release`, and `db/scripts`.
These immutable scripts must be safely rerunnable; formatting does not prove that
contract. Never reformat a script already recorded as executed or skipped.

Run `make format-sql` to apply SQLFluff's safe formatting fixes only to desired
schemas; format new migration and data scripts explicitly before their first
release. Run
`make lint-sql` to validate the disposable PostgreSQL load, native rules,
Schemalint rules, and formatting together.
