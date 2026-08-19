#!/bin/sh
# Assembles pgBackRest's configuration, then hands over to the official
# entrypoint unchanged.
#
# Why this exists at all, and why it is not a wrapper around the pgbackrest
# binary: pgBackRest is invoked along three paths that share nothing.
#
#   1. The server's archive_command, a child of postmaster.
#   2. The restore_command pgBackRest writes into postgresql.auto.conf during a
#      restore. That line is generated as the *absolute resolved path* of the
#      real binary and cannot be configured, so no wrapper — by name, by PATH,
#      or by argv[0] — is reachable from it.
#   3. `docker exec ... pgbackrest`, which inherits the container's configured
#      environment but nothing an entrypoint exported.
#
# A credential passed through the environment covers 1 and 3 and misses 2, which
# means it works until the moment it is needed. So the credentials are put where
# every path finds them regardless of who invoked the binary or how: pgBackRest's
# own configuration, in the include directory it reads by default.
#
# Onebox's generated configuration is mounted read-only elsewhere and symlinked
# in rather than copied, so regenerating it on the host takes effect without
# rebuilding or restarting anything.
set -eu

ob_indirect() {
    # POSIX sh has no ${!name}. eval on a name already checked against the
    # variable-name grammar is the portable equivalent.
    case "$1" in
        [A-Za-z_][A-Za-z0-9_]*) ;;
        *) echo "ob: $1 is not a variable name" >&2; exit 1 ;;
    esac
    eval "printf '%s' \"\${$1-}\""
}

if [ -f /etc/onebox/pgbackrest.conf ]; then
    mkdir -p /etc/pgbackrest/conf.d
    ln -sfn /etc/onebox/pgbackrest.conf /etc/pgbackrest/pgbackrest.conf

    # Written with a restrictive umask *before* any content, so the file is
    # never briefly readable with the credentials already in it.
    credentials=/etc/pgbackrest/conf.d/ob-credentials.conf
    (
        umask 077
        {
            echo "# Generated at container start from the credential file on the host."
            echo "[global]"
            [ -n "${OB_S3_KEY_ENTRY-}" ] && \
                echo "repo1-s3-key=$(ob_indirect "$OB_S3_KEY_ENTRY")"
            [ -n "${OB_S3_SECRET_ENTRY-}" ] && \
                echo "repo1-s3-key-secret=$(ob_indirect "$OB_S3_SECRET_ENTRY")"
            [ -n "${OB_S3_SESSION_TOKEN_ENTRY-}" ] && \
                echo "repo1-s3-token=$(ob_indirect "$OB_S3_SESSION_TOKEN_ENTRY")"
            # The repository passphrase has a fixed name: it is Onebox's own
            # requirement rather than a property of the destination, so there is
            # no backup_targets field to indirect through. The OB_ prefix is not
            # decoration — pgBackRest reads every PGBACKREST_<OPTION> variable as
            # an option, so this name under that prefix becomes a malformed
            # option on every command.
            [ -n "${OB_REPOSITORY_PASSPHRASE-}" ] && \
                echo "repo1-cipher-pass=$OB_REPOSITORY_PASSPHRASE"
            true
        } > "$credentials"
    )
    chown postgres:postgres "$credentials"
    chmod 0600 "$credentials"
fi

exec /usr/local/bin/docker-entrypoint.sh "$@"
