# syntax=docker/dockerfile:1

ARG PG_MAJOR=18
ARG DEBIAN_CODENAME=trixie
FROM postgres:${PG_MAJOR}-${DEBIAN_CODENAME}

ARG PG_MAJOR
ARG TARGETARCH
ARG PGVECTOR_COMMIT=8ee86c96f0fd72390f890aa8a336fda6d3ab4c6c
ARG PGVECTOR_VERSION=0.8.6
ARG PGVECTORSCALE_COMMIT=c66cae4b621664b68546587da9fafd80b791e643
ARG PGVECTORSCALE_VERSION=0.9.0
ARG PGVECTORSCALE_AMD64_SHA256=7a5450b81a7403ca20ff5e5a2f81aa13c81795ddd1fdfe9b986c42c48b12ed67
ARG PGVECTORSCALE_ARM64_SHA256=8d0916df999f082ceb3d019bdfa72f5df395c31b152f7906de59e429ee11edc7
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
        ca-certificates \
        curl \
        postgresql-server-dev-${PG_MAJOR} \
        postgresql-${PG_MAJOR}-postgis-3=${POSTGIS_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-cron=${PG_CRON_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-pgaudit=${PGAUDIT_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-repack=${PG_REPACK_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-partman=${PG_PARTMAN_PACKAGE_VERSION} \
        postgresql-${PG_MAJOR}-hypopg=${HYPOPG_PACKAGE_VERSION} \
        unzip && \
    cd /tmp/pgvector && \
    make clean && \
    make OPTFLAGS="" && \
    make install && \
    grep -Fqx "default_version = '${PGVECTOR_VERSION}'" vector.control && \
    install -d /usr/share/doc/pgvector && \
    install -m 0644 LICENSE README.md /usr/share/doc/pgvector/ && \
    printf '%s\n' "${PGVECTOR_VERSION}" > /usr/share/doc/pgvector/VERSION && \
    case "${TARGETARCH}" in \
        amd64) pgvectorscale_sha="${PGVECTORSCALE_AMD64_SHA256}" ;; \
        arm64) pgvectorscale_sha="${PGVECTORSCALE_ARM64_SHA256}" ;; \
        *) echo "unsupported pgvectorscale architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    pgvectorscale_archive="pgvectorscale-${PGVECTORSCALE_VERSION}-pg${PG_MAJOR}-${TARGETARCH}.zip" && \
    curl -fsSL \
        "https://github.com/timescale/pgvectorscale/releases/download/${PGVECTORSCALE_VERSION}/${pgvectorscale_archive}" \
        -o "/tmp/${pgvectorscale_archive}" && \
    echo "${pgvectorscale_sha}  /tmp/${pgvectorscale_archive}" | sha256sum -c - && \
    install -d /tmp/pgvectorscale-package && \
    unzip -j "/tmp/${pgvectorscale_archive}" \
        "pgvectorscale-postgresql-${PG_MAJOR}_${PGVECTORSCALE_VERSION}-Linux_${TARGETARCH}.deb" \
        -d /tmp/pgvectorscale-package && \
    apt-get install -y --no-install-recommends \
        "/tmp/pgvectorscale-package/pgvectorscale-postgresql-${PG_MAJOR}_${PGVECTORSCALE_VERSION}-Linux_${TARGETARCH}.deb" && \
    grep -Fqx "default_version = '${PGVECTORSCALE_VERSION}'" \
        "/usr/share/postgresql/${PG_MAJOR}/extension/vectorscale.control" && \
    install -d /usr/share/doc/pgvectorscale && \
    curl -fsSL \
        "https://raw.githubusercontent.com/timescale/pgvectorscale/${PGVECTORSCALE_COMMIT}/LICENSE" \
        -o /usr/share/doc/pgvectorscale/LICENSE && \
    curl -fsSL \
        "https://raw.githubusercontent.com/timescale/pgvectorscale/${PGVECTORSCALE_COMMIT}/NOTICE" \
        -o /usr/share/doc/pgvectorscale/NOTICE && \
    echo "df34f0384d53261f4dc47b3d834a13b570f177b8ab1c8a00266a98f30de2e117  /usr/share/doc/pgvectorscale/LICENSE" | sha256sum -c - && \
    echo "4e204f7b0aa175af0a3b38c3bc56852954adf0110e25babe94eab6e35eeef114  /usr/share/doc/pgvectorscale/NOTICE" | sha256sum -c - && \
    cd / && \
    rm -rf /tmp/pgvector /tmp/pgvectorscale-package "/tmp/${pgvectorscale_archive}" && \
    apt-get remove -y build-essential ca-certificates curl postgresql-server-dev-${PG_MAJOR} unzip && \
    apt-get autoremove -y && \
    apt-mark unhold locales && \
    rm -rf /var/lib/apt/lists/*
