#!/bin/sh
# Starts PgBouncer from the pgbouncer.ini beside this script. It replaces the
# image's own entrypoint, which rewrites files under /etc/pgbouncer and so
# cannot run against a read-only config mount. Everything it writes goes under
# /tmp/pgbouncer.
#
#   PGBOUNCER_DB_HOST      Postgres host to forward to (required)
#   PGBOUNCER_RESOLVE_HOST 1 to resolve that host once at start (see below)
#   PGBOUNCER_DB_PORT      its port (default 5432)
#   PGBOUNCER_DB_NAME      database to expose (default "*": any database name
#                          is forwarded to the database of the same name)
#   PGBOUNCER_DB_USER      the one user allowed through (default kiln)
#   PGBOUNCER_DB_PASSWORD  its password (required)
#   PGBOUNCER_SERVER_TLS_SSLMODE
#                          TLS to Postgres (default prefer; require or
#                          verify-full for a Postgres reached over a network)
#   PGBOUNCER_SET          space-separated key=value overrides of the
#                          [pgbouncer] section, e.g. "listen_port=56432".
#                          Only sizing and timeout keys are accepted: auth,
#                          pool mode and packet limits stay as the file says.
#
# Used by docker-compose.yml, `make test-pooled` and CI. The Helm chart builds
# the same files with an init container instead (templates/pooler.yaml).
set -eu

: "${PGBOUNCER_DB_HOST:?set PGBOUNCER_DB_HOST}"
: "${PGBOUNCER_DB_PASSWORD:?set PGBOUNCER_DB_PASSWORD}"
port=${PGBOUNCER_DB_PORT:-5432}
name=${PGBOUNCER_DB_NAME:-*}
user=${PGBOUNCER_DB_USER:-kiln}
here=$(dirname "$0")
out=/tmp/pgbouncer

umask 077
mkdir -p "$out"

# PgBouncer resolves server hosts with its own DNS client, not libc, and that
# client fails on some names libc resolves -- Docker Desktop's
# host.docker.internal among them. PGBOUNCER_RESOLVE_HOST=1 resolves the host
# once here and pins its address. Leave it off for ordinary DNS names (compose
# service names, managed database endpoints): PgBouncer re-resolves those as
# their addresses change, and a pinned address would not follow.
host=$PGBOUNCER_DB_HOST
if [ "${PGBOUNCER_RESOLVE_HOST:-}" = 1 ]; then
	# IPv4 first: Docker Desktop also publishes an IPv6 address for
	# host.docker.internal that containers cannot route to.
	host=$(getent ahostsv4 "$PGBOUNCER_DB_HOST" 2>/dev/null | awk '{ print $1; exit }')
	[ -n "$host" ] || host=$(getent hosts "$PGBOUNCER_DB_HOST" | awk '{ print $1; exit }')
	: "${host:?cannot resolve $PGBOUNCER_DB_HOST}"
fi

if [ "$name" = "*" ]; then
	printf '* = host=%s port=%s\n' "$host" "$port" >"$out/databases.ini"
else
	printf '%s = host=%s port=%s dbname=%s\n' "$name" "$host" "$port" "$name" >"$out/databases.ini"
fi

# userlist.txt quotes each field and doubles any quote inside it.
quote() { printf '"%s"' "$(printf '%s' "$1" | sed 's/"/""/g')"; }
printf '%s %s\n' "$(quote "$user")" "$(quote "$PGBOUNCER_DB_PASSWORD")" >"$out/userlist.txt"

# The shared file ends in the [pgbouncer] section, and a later key wins, so
# overrides are appended rather than edited in.
cp "$here/pgbouncer.ini" "$out/pgbouncer.ini"
printf 'server_tls_sslmode = %s\n' "${PGBOUNCER_SERVER_TLS_SSLMODE:-prefer}" >>"$out/pgbouncer.ini"
set -f # split PGBOUNCER_SET on spaces, but never glob it
for kv in ${PGBOUNCER_SET:-}; do
	key=${kv%%=*}
	case "$key" in
	listen_port | default_pool_size | reserve_pool_size | min_pool_size | max_client_conn | query_wait_timeout | server_tls_sslmode | max_prepared_statements) ;;
	*)
		echo "start.sh: PGBOUNCER_SET may not override $key" >&2
		exit 64
		;;
	esac
	printf '%s = %s\n' "$key" "${kv#*=}" >>"$out/pgbouncer.ini"
done

exec /usr/bin/pgbouncer "$out/pgbouncer.ini"
