# Pin Glide's PostgreSQL server distribution: the official PostgreSQL 18 Debian
# (trixie) image plus pg_cron, which scheduled database work requires. Every
# database consumer -- Compose, the disposable lint/codegen loader and the
# integration harness -- must use this image, because the base image cannot load
# the extension at all.
FROM postgres:18

# Source: the PGDG apt repository the official image already configures.
# Version: pg_cron 1.6.8 for PostgreSQL 18 (trixie-pgdg).
RUN apt-get update \
 && apt-get install --yes --no-install-recommends postgresql-18-cron=1.6.8-1.pgdg13+2 \
 && rm -rf /var/lib/apt/lists/*

# initdb copies this sample into a new data directory, so every consumer
# preloads pg_cron without repeating command-line configuration. pg_cron
# requires the preload, and it schedules jobs in one named database.
RUN printf '\nshared_preload_libraries = %s\ncron.database_name = %s\n' \
      "'pg_cron'" "'pin_glide'" >> /usr/share/postgresql/18/postgresql.conf.sample
