# Local PostgreSQL needs Anonymizer 3.2.3 and Pin Glide's pinned pg_cron 1.6.8
# on PostgreSQL 18. pg_cron stays preloaded for extension compatibility, but its
# launcher is disabled independently of every restored or Data Script job state.
FROM registry.gitlab.com/dalibo/postgresql_anonymizer:3.2.3

RUN apt-get update \
 && apt-get install --yes --no-install-recommends postgresql-18-cron=1.6.8-1.pgdg13+2 \
 && rm -rf /var/lib/apt/lists/* \
 && printf '\nshared_preload_libraries = %s\ncron.database_name = %s\ncron.launch_active_jobs = off\n' \
      "'pg_cron'" "'pin_glide'" >> /usr/share/postgresql/18/postgresql.conf.sample \
 && printf '%s\n' \
      '#!/bin/sh' \
      'set -eu' \
      'psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL' \
      "ALTER SYSTEM SET shared_preload_libraries TO 'anon', 'pg_cron';" \
      "ALTER SYSTEM SET cron.database_name = 'pin_glide';" \
      "ALTER SYSTEM SET cron.launch_active_jobs = 'off';" \
      'EOSQL' \
      > /docker-entrypoint-initdb.d/zz-pin-glide.sh \
 && chmod +x /docker-entrypoint-initdb.d/zz-pin-glide.sh
