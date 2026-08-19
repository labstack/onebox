# The protected PostgreSQL image.
#
# Onebox runs pgBackRest in the same container as the server, because that is
# what physical backup requires: pgBackRest reads and writes the data directory
# directly, and the server's archive_command invokes it synchronously for every
# completed WAL segment. A sidecar cannot do either.
#
# The base is the official image at an exact major, and pgBackRest comes from
# PGDG — the same apt repository the base image already trusts for the server.
# The Alpine package was tried first and rejected: its pgbackrest pulls in a
# complete second PostgreSQL server (18) next to the 17 the image runs, so the
# image would ship two majors and a `postgresql-common` default pointing at the
# wrong one.
ARG POSTGRES_VERSION=17
FROM postgres:${POSTGRES_VERSION}-bookworm

# The base image already has the PGDG repository and its key configured, which
# is what makes this a one-package addition rather than a repository setup.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends pgbackrest; \
    rm -rf /var/lib/apt/lists/*; \
    pgbackrest version

# pgBackRest's runtime directories must exist and be owned by postgres before
# the first archive-push: it creates none of them, and the failure surfaces
# later as a WAL archive that silently never drains.
RUN set -eux; \
    mkdir -p /etc/pgbackrest/conf.d /etc/onebox /var/log/pgbackrest /var/spool/pgbackrest; \
    chown -R postgres:postgres /etc/pgbackrest /var/log/pgbackrest /var/spool/pgbackrest; \
    chmod 0750 /var/log/pgbackrest /var/spool/pgbackrest

COPY ob-protected-entrypoint.sh /usr/local/bin/ob-protected-entrypoint.sh
RUN chmod 0755 /usr/local/bin/ob-protected-entrypoint.sh

ENTRYPOINT ["/usr/local/bin/ob-protected-entrypoint.sh"]
CMD ["postgres"]
