# Anonymizer label examples

Choose a pattern by the transformation you need. Tables and columns below are
fictional; adapt them to your schema and its constraints. See
[file naming](../../docs/sql-conventions.md#local-anonymizer-labels).

## Replace with a constant

Use `MASKED WITH VALUE` for booleans, numbers, strings, or a whole JSON value.
Dollar quoting keeps strings readable inside the label.

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.enabled
IS 'MASKED WITH VALUE false';

SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.base_url
IS 'MASKED WITH VALUE $$http://127.0.0.1:9$$';

SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.port
IS 'MASKED WITH VALUE 9';

SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.headers
IS 'MASKED WITH VALUE $${}$$';
```

The last example assumes a `jsonb` column; PostgreSQL converts the literal to
the column type.

## Clear a nullable column

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.optional_secret
IS 'MASKED WITH VALUE NULL';
```

Use this only where `NULL` satisfies the column and table constraints.

## Generate new values

Use `MASKED WITH FUNCTION` for generated names or bounded numeric values.

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_profiles.display_name
IS 'MASKED WITH FUNCTION anon.dummy_first_name()';

SECURITY LABEL FOR anon ON COLUMN pin_glide.example_profiles.score
IS 'MASKED WITH FUNCTION anon.random_int_between(0,100)';
```

Results can change each time masking runs; generated values can collide.

## Generate a stable replacement

Use an unchanged text column as the seed to get repeatable results.

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_profiles.display_name
IS 'MASKED WITH FUNCTION
    anon.seeded_first_name(subject_key, $$en_US$$, $$local-dev$$)';
```

The same seed, locale, salt, and fake dataset produce the same name.
Different seeds can still produce the same name.

## Hash a value

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_profiles.external_reference
IS 'MASKED WITH FUNCTION anon.hash(external_reference)';
```

Keep `anon.salt` and `anon.algorithm` consistent when matching values across
tables. Hashing the already masked value changes it again; allow enough column
length for the result.

## Keep part of a string

Keep the last four characters and replace the rest:

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_profiles.phone
IS 'MASKED WITH FUNCTION anon.partial(phone,0,$$***$$,4)';
```

## Apply a condition

Preserve missing values while replacing populated values:

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_profiles.note
IS 'MASKED WITH FUNCTION anon.ternary(note IS NULL, NULL::text, $$local$$)';
```

## Replace one JSON field

For a `jsonb` object, replace `token` with JSON `null` while keeping other keys.
The final `false` leaves objects without that key unchanged.

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.settings
IS 'MASKED WITH FUNCTION
    pg_catalog.jsonb_set(settings, $${token}$$, $$null$$, false)';
```

Masking functions must be trusted. Inspect the built-in function list:

```sql
SELECT * FROM anon.pg_trusted_functions;
```

If needed, a superuser can trust this specific function in the local database
before applying the column label:

```sql
SECURITY LABEL FOR anon ON FUNCTION
pg_catalog.jsonb_set(jsonb, text[], jsonb, boolean)
IS 'TRUSTED';
```

Custom functions use the same `MASKED WITH FUNCTION` form. Create the helper and
trust its exact signature during local setup; table label files only declare
the masks. The current launcher does not create custom helpers.

## Change or remove a rule

Running another label statement for the same column replaces its rule. To
remove the rule:

```sql
SECURITY LABEL FOR anon ON COLUMN pin_glide.example_configs.base_url IS NULL;
```

This removes the rule, not the column value, and does not undo static masking.
Inspect the stored rules with:

```sql
SELECT * FROM anon.all_rules;
```

These are alternative examples: choose one rule per column. They cover value
transformations; retaining part of a value or generating a fake value does not
by itself prevent external calls.

References: [label syntax](https://postgresql-anonymizer.readthedocs.io/en/stable/declare_masking_rules/),
[masking functions](https://postgresql-anonymizer.readthedocs.io/en/stable/masking_functions/).
