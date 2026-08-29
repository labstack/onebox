# syntax=docker/dockerfile:1

ARG PG_MAJOR=18
ARG DEBIAN_CODENAME=trixie
FROM postgres:${PG_MAJOR}-${DEBIAN_CODENAME}

ARG PG_MAJOR
ARG PGVECTOR_COMMIT=8ee86c96f0fd72390f890aa8a336fda6d3ab4c6c
ARG PGVECTOR_VERSION=0.8.6
ARG POSTGIS_PACKAGE_VERSION=3.6.4+dfsg-2.pgdg13+1
ARG PG_CRON_PACKAGE_VERSION=1.6.7-3.pgdg13+1
ARG PGAUDIT_PACKAGE_VERSION=18.0-3.pgdg13+1
ARG PG_REPACK_PACKAGE_VERSION=1.5.3-1.pgdg13+1
ARG PG_PARTMAN_PACKAGE_VERSION=5.5.0-1.pgdg13+1
ARG HYPOPG_PACKAGE_VERSION=1.4.3-1.pgdg13+1

LABEL org.opencontainers.image.source="https://github.com/labstack/onebox" \
      org.opencontainers.image.description="PostgreSQL for Onebox-managed applications" \
      org.opencontainers.image.licenses="PostgreSQL AND GPL-2.0-or-later"

# The full commit is immutable. BuildKit checks out that exact source instead
# of trusting a movable release tag, and the final image contains no compiler.
ADD https://github.com/pgvector/pgvector.git#${PGVECTOR_COMMIT} /tmp/pgvector
RUN apt-get update && \
    apt-mark hold locales && \
    apt-get install -y --no-install-recommends \
        build-essential \
        postgresql-server-dev-${PG_MAJOR} \
        postgresql-${PG_MAJOR}-postgis-3=${POSTGIS_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-cron=${PG_CRON_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-pgaudit=${PGAUDIT_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-repack=${PG_REPACK_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-partman=${PG_PARTMAN_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-hypopg=${HYPOPG_PACKAGE_VERSION} && \
    cd /tmp/pgvector && \
    make clean && \
    make OPTFLAGS="" && \
    make install && \
    grep -Fqx "default_version = '${PGVECTOR_VERSION}'" vector.control && \
    install -d /usr/share/doc/pgvector && \
    install -m 0644 LICENSE README.md /usr/share/doc/pgvector/ && \
    printf '%s\n' "${PGVECTOR_VERSION}" > /usr/share/doc/pgvector/VERSION && \
    cd / && \
    rm -rf /tmp/pgvector && \
    apt-get remove -y build-essential postgresql-server-dev-${PG_MAJOR} && \
    apt-get autoremove -y && \
    apt-mark unhold locales && \
    rm -rf /var/lib/apt/lists/*
