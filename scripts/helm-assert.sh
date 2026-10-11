#!/usr/bin/env bash
# Assertions over `helm template` output for deploy/helm/kiln. CI has no
# cluster to deploy the chart to, so what each values combination renders is
# checked here instead: the pooler wiring, the migrate Job staying direct, no
# credential in a ConfigMap, the scheduler and the workers' duty flag, the
# refusals, and a kubeconform schema pass.
#
#   scripts/helm-assert.sh        (also: make helm-test)
#
# Needs helm and go. Plain awk/grep over the rendered YAML, so no yq.
set -euo pipefail

cd "$(dirname "$0")/.."
chart=deploy/helm/kiln
# Pinned: a schema checker that moves under CI is a flaky check.
kubeconform="go run github.com/yannh/kubeconform/cmd/kubeconform@v0.7.0"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}
pass() { echo "ok:   $*"; }

render() { # out-file, then --set flags
	local out=$1
	shift
	helm template kiln "$chart" --namespace kiln --set secrets.existingSecret=kiln-secrets "$@" >"$out"
}

# doc KIND NAME FILE prints the one rendered document of that kind and name.
doc() {
	awk -v kind="$1" -v name="$2" '
		function flush() { if (k == kind && n == name) printf "%s", buf; buf = ""; k = ""; n = ""; meta = 0 }
		/^---/ { flush(); next }
		{ buf = buf $0 "\n" }
		/^kind: / { k = $2 }
		/^metadata:/ { meta = 1; next }
		meta && /^  name: / { n = $2; meta = 0 }
		/^[a-z]/ && !/^metadata:/ { meta = 0 }
		END { flush() }
	' "$3"
}

# envval NAME prints the value of env var NAME from a document on stdin.
envval() {
	awk -v want="$1" '
		$0 ~ "- name: " want "$" { getline; sub(/^ *value: /, ""); gsub(/"/, ""); print; exit }
	'
}

has() { grep -q -- "$1"; }

expect_env() { # label, doc-file, var, want
	local got
	got=$(envval "$3" <"$2")
	if [ "$got" = "$4" ]; then pass "$1: $3=$4"; else fail "$1: $3=${got:-<unset>}, want $4"; fi
}

expect_no_env() { # label, doc-file, var
	if grep -q -- "- name: $3\$" "$2"; then fail "$1: $3 is set"; else pass "$1: no $3"; fi
}

# expect_quoted label doc-file var want: the raw value line is the quoted
# string, as a bool env value must be or the Deployment does not apply.
expect_quoted() {
	if awk -v want="$3" -v val="value: \"$4\"" '
		$0 ~ "- name: " want "$" { getline; sub(/^ */, ""); found = ($0 == val); exit }
		END { exit !found }
	' "$2"; then pass "$1: $3=\"$4\""; else fail "$1: $3 is not \"$4\""; fi
}

# --- 1. default render: no pooler, nothing changed for unpooled users --------
render "$tmp/default.yaml" --set config.database.host=pg
for kind in Deployment Service ConfigMap PodDisruptionBudget Secret; do
	if [ -n "$(doc "$kind" kiln-pooler "$tmp/default.yaml")" ]; then fail "default: renders $kind/kiln-pooler"; fi
done
if grep -q 'pgbouncer' "$tmp/default.yaml"; then fail "default: mentions pgbouncer"; else pass "default: no pooler objects"; fi
doc Deployment kiln-api "$tmp/default.yaml" >"$tmp/d-api"
doc Deployment kiln-worker "$tmp/default.yaml" >"$tmp/d-worker"
doc Job kiln-migrate "$tmp/default.yaml" >"$tmp/d-migrate"
for d in api worker migrate; do
	expect_env "default $d" "$tmp/d-$d" KILN_DATABASE_HOST pg
	expect_no_env "default $d" "$tmp/d-$d" KILN_DATABASE_QUERY_EXEC_MODE
	if has 'key: database-url' <"$tmp/d-$d"; then pass "default $d: database-url still honoured"; else fail "default $d: database-url dropped"; fi
done
expect_env "default api" "$tmp/d-api" KILN_DATABASE_MAX_CONNS 20
expect_env "default worker" "$tmp/d-worker" KILN_DATABASE_MAX_CONNS 6
if has 'initContainers' <"$tmp/d-api"; then fail "default api: has initContainers"; fi

# --- 2. pooler enabled ------------------------------------------------------
render "$tmp/pooled.yaml" --set pooler.enabled=true --set config.database.host=pg
for kind in Deployment Service ConfigMap PodDisruptionBudget; do
	if [ -n "$(doc "$kind" kiln-pooler "$tmp/pooled.yaml")" ]; then pass "pooled: $kind/kiln-pooler"; else fail "pooled: no $kind/kiln-pooler"; fi
done
if [ -n "$(doc Secret kiln-pooler-tls "$tmp/pooled.yaml")" ]; then pass "pooled: generated TLS secret"; else fail "pooled: no TLS secret"; fi

doc Deployment kiln-api "$tmp/pooled.yaml" >"$tmp/p-api"
doc Deployment kiln-worker "$tmp/pooled.yaml" >"$tmp/p-worker"
doc Job kiln-migrate "$tmp/pooled.yaml" >"$tmp/p-migrate"
doc Deployment kiln-pooler "$tmp/pooled.yaml" >"$tmp/p-pooler"
doc ConfigMap kiln-pooler "$tmp/pooled.yaml" >"$tmp/p-ini"
for d in api worker; do
	expect_env "pooled $d" "$tmp/p-$d" KILN_DATABASE_HOST kiln-pooler
	expect_env "pooled $d" "$tmp/p-$d" KILN_DATABASE_PORT 6432
	expect_env "pooled $d" "$tmp/p-$d" KILN_DATABASE_QUERY_EXEC_MODE cache_describe
	expect_no_env "pooled $d" "$tmp/p-$d" KILN_DATABASE_URL
	if has 'name: wait-for-pooler' <"$tmp/p-$d"; then pass "pooled $d: waits for the pooler"; else fail "pooled $d: no wait-for-pooler"; fi
done
expect_env "pooled api" "$tmp/p-api" KILN_DATABASE_MAX_CONNS 8
expect_env "pooled worker" "$tmp/p-worker" KILN_DATABASE_MAX_CONNS 4
# The migrate hook runs before the pooler exists, so it must go direct.
expect_env "pooled migrate" "$tmp/p-migrate" KILN_DATABASE_HOST pg
expect_no_env "pooled migrate" "$tmp/p-migrate" KILN_DATABASE_QUERY_EXEC_MODE
# The image entrypoint rewrites /etc/pgbouncer, which is read-only here.
if has '/usr/bin/pgbouncer /etc/pgbouncer/pgbouncer.ini' <"$tmp/p-pooler"; then pass "pooler: entrypoint overridden"; else fail "pooler: entrypoint not overridden"; fi
if has '@sha256:' <"$tmp/p-pooler"; then pass "pooler: image pinned by digest"; else fail "pooler: image not pinned by digest"; fi
if has 'checksum/config:' <"$tmp/p-pooler"; then pass "pooler: config checksum annotation"; else fail "pooler: no config checksum"; fi
# kubelet's SIGTERM means "wait for clients" to PgBouncer, and a second one
# "quit now"; it must reach PgBouncer as SIGINT, and the container must wait
# for PgBouncer to exit.
if has "trap 'kill -INT \$p' TERM" <"$tmp/p-pooler" && has 'wait \$p; wait \$p' <"$tmp/p-pooler"; then
	pass "pooler: SIGTERM forwarded as SIGINT, waits for exit"
else
	fail "pooler: SIGTERM not forwarded as SIGINT"
fi
if has 'command: \["sleep", "5"\]' <"$tmp/p-pooler"; then pass "pooler: preStop sleep"; else fail "pooler: no preStop sleep"; fi

# Security-relevant settings agree with the file compose and CI run.
for key in pool_mode auth_type max_packet_size max_prepared_statements ignore_startup_parameters client_login_timeout; do
	want=$(awk -F' *= *' -v k="$key" '$1 == k { print $2 }' deploy/pgbouncer/pgbouncer.ini)
	got=$(awk -F' *= *' -v k="$key" '{ sub(/^ +/, "", $1) } $1 == k { print $2 }' "$tmp/p-ini")
	if [ -n "$want" ] && [ "$want" = "$got" ]; then pass "ini parity: $key = $want"; else fail "ini parity: $key chart=${got:-<unset>} shared=${want:-<unset>}"; fi
done

# Server-side TLS is the one deliberate difference: compose's Postgres has
# none, a cluster's Postgres should.
got=$(awk -F' *= *' '{ sub(/^ +/, "", $1) } $1 == "server_tls_sslmode" { print $2 }' "$tmp/p-ini")
if [ "$got" = require ]; then pass "chart: server_tls_sslmode = require"; else fail "chart: server_tls_sslmode=${got:-<unset>}, want require"; fi

# One replica: no PDB, which would block every drain.
render "$tmp/single.yaml" --set pooler.enabled=true --set config.database.host=pg --set pooler.replicas=1
if [ -z "$(doc PodDisruptionBudget kiln-pooler "$tmp/single.yaml")" ]; then pass "single pooler replica: no PDB"; else fail "single pooler replica: PDB rendered"; fi

# No credential in any ConfigMap: render with an inline password and find it
# only inside Secret objects.
render "$tmp/inline.yaml" --set pooler.enabled=true --set config.database.host=pg \
	--set secrets.existingSecret= --set secrets.create=true --set secrets.masterKey=mk \
	--set secrets.databasePassword=Sentinel-Pw-7731
leaks=$(awk '/^---/ { kind = "" } /^kind: / { kind = $2 } /Sentinel-Pw-7731/ && kind != "Secret" { print kind }' "$tmp/inline.yaml")
if [ -z "$leaks" ] && grep -q Sentinel-Pw-7731 "$tmp/inline.yaml"; then pass "password only in Secret objects"; else fail "password rendered outside a Secret: ${leaks:-<nowhere>}"; fi
# A chart-owned password rolls the pooler when it changes.
if doc Deployment kiln-pooler "$tmp/inline.yaml" | has 'checksum/password:'; then pass "pooler: password checksum annotation"; else fail "pooler: no password checksum"; fi

# A managed certificate replaces the generated one.
render "$tmp/certs.yaml" --set pooler.enabled=true --set config.database.host=pg --set pooler.tls.secretName=managed-cert
if [ -z "$(doc Secret kiln-pooler-tls "$tmp/certs.yaml")" ] && has 'secretName: managed-cert' <"$tmp/certs.yaml"; then
	pass "pooler.tls.secretName used, nothing generated"
else
	fail "pooler.tls.secretName not honoured"
fi

# --- 3. scheduler -----------------------------------------------------------
# On by default: one scheduler pod, and the workers told to leave the duties
# to it in the same release.
doc Deployment kiln-scheduler "$tmp/default.yaml" >"$tmp/d-scheduler"
if [ "$(grep -c '^kind: Deployment' "$tmp/d-scheduler")" = 1 ]; then pass "default: one scheduler Deployment"; else fail "default: not exactly one scheduler Deployment"; fi
if has 'replicas: 1$' <"$tmp/d-scheduler"; then pass "scheduler: replicas 1"; else fail "scheduler: replicas not 1"; fi
if has 'type: Recreate' <"$tmp/d-scheduler"; then pass "scheduler: Recreate"; else fail "scheduler: not Recreate"; fi
if has 'args: \["scheduler"\]' <"$tmp/d-scheduler"; then pass "scheduler: runs kiln scheduler"; else fail "scheduler: wrong args"; fi
for kind in PodDisruptionBudget HorizontalPodAutoscaler; do
	if [ -n "$(doc "$kind" kiln-scheduler "$tmp/default.yaml")" ]; then fail "scheduler: renders a $kind"; fi
done
expect_quoted "default worker" "$tmp/d-worker" KILN_WORKER_SCHEDULER_DUTIES false
expect_no_env "default scheduler" "$tmp/d-scheduler" KILN_WORKER_SCHEDULER_DUTIES
# Database and object storage only: no key that opens credentials or spends.
for v in KILN_MASTER_KEY KILN_ANTHROPIC_API_KEY KILN_GITHUB_PRIVATE_KEY KILN_GITHUB_CLIENT_SECRET KILN_GITHUB_WEBHOOK_SECRET; do
	expect_no_env "scheduler" "$tmp/d-scheduler" "$v"
done
if has 'key: database-password' <"$tmp/d-scheduler" && has 'key: storage-secret-key' <"$tmp/d-scheduler"; then
	pass "scheduler: database and storage secrets"
else
	fail "scheduler: missing database or storage secret"
fi
expect_env "default scheduler" "$tmp/d-scheduler" KILN_DATABASE_HOST pg
expect_env "default scheduler" "$tmp/d-scheduler" KILN_DATABASE_MAX_CONNS 2
expect_env "default scheduler" "$tmp/d-scheduler" KILN_WORKER_STALE_AFTER "$(envval KILN_WORKER_STALE_AFTER <"$tmp/d-worker")"
if has 'path: /readyz' <"$tmp/d-scheduler" && has 'path: /healthz' <"$tmp/d-scheduler"; then
	pass "scheduler: readiness and liveness probes"
else
	fail "scheduler: probes missing"
fi
# Through the pooler like the API and workers.
doc Deployment kiln-scheduler "$tmp/pooled.yaml" >"$tmp/p-scheduler"
expect_env "pooled scheduler" "$tmp/p-scheduler" KILN_DATABASE_HOST kiln-pooler
expect_env "pooled scheduler" "$tmp/p-scheduler" KILN_DATABASE_QUERY_EXEC_MODE cache_describe
expect_no_env "pooled scheduler" "$tmp/p-scheduler" KILN_DATABASE_URL
if has 'name: wait-for-pooler' <"$tmp/p-scheduler"; then pass "pooled scheduler: waits for the pooler"; else fail "pooled scheduler: no wait-for-pooler"; fi

# Disabled: nothing rendered, and the workers keep the duties.
render "$tmp/nosched.yaml" --set config.database.host=pg --set scheduler.enabled=false
if [ -z "$(doc Deployment kiln-scheduler "$tmp/nosched.yaml")" ]; then pass "scheduler.enabled=false: no scheduler"; else fail "scheduler.enabled=false: scheduler rendered"; fi
doc Deployment kiln-worker "$tmp/nosched.yaml" >"$tmp/n-worker"
expect_quoted "scheduler.enabled=false worker" "$tmp/n-worker" KILN_WORKER_SCHEDULER_DUTIES true

# worker.schedulerDuties overrides auto, for the two-step upgrade (true while
# the scheduler comes up), and refuses anything but auto/true/false.
render "$tmp/staged.yaml" --set config.database.host=pg --set worker.schedulerDuties=true
doc Deployment kiln-worker "$tmp/staged.yaml" >"$tmp/s-worker"
expect_quoted "worker.schedulerDuties=true worker" "$tmp/s-worker" KILN_WORKER_SCHEDULER_DUTIES true
if [ -n "$(doc Deployment kiln-scheduler "$tmp/staged.yaml")" ]; then pass "worker.schedulerDuties=true: scheduler still rendered"; else fail "worker.schedulerDuties=true: scheduler dropped"; fi
render "$tmp/forced.yaml" --set config.database.host=pg --set scheduler.enabled=false --set worker.schedulerDuties=false
doc Deployment kiln-worker "$tmp/forced.yaml" >"$tmp/f-worker"
expect_quoted "worker.schedulerDuties=false worker" "$tmp/f-worker" KILN_WORKER_SCHEDULER_DUTIES false
if out=$(helm template kiln "$chart" --set secrets.existingSecret=kiln-secrets --set worker.schedulerDuties=sometimes 2>&1); then
	fail "worker.schedulerDuties=sometimes rendered"
elif grep -q 'must be auto, true or false' <<<"$out"; then
	pass "worker.schedulerDuties=sometimes refused"
else
	fail "worker.schedulerDuties=sometimes refused without saying why: $out"
fi

# --- 4. refusals ------------------------------------------------------------
if out=$(helm template kiln "$chart" --set secrets.existingSecret=kiln-secrets --set pooler.enabled=true \
	--set config.database.host=pg --set secrets.databaseURL=postgres://x 2>&1); then
	fail "pooler + secrets.databaseURL rendered"
elif grep -q bypass <<<"$out"; then
	pass "pooler + secrets.databaseURL refused (bypass)"
else
	fail "pooler + secrets.databaseURL refused without saying why: $out"
fi
if out=$(helm template kiln "$chart" --set secrets.existingSecret=kiln-secrets --set pooler.enabled=true 2>&1); then
	fail "pooler without config.database.host rendered"
elif grep -q 'config.database.host' <<<"$out"; then
	pass "pooler without config.database.host refused"
else
	fail "pooler without host refused without saying why: $out"
fi

# --- 5. schemas -------------------------------------------------------------
if $kubeconform -strict -summary "$tmp/default.yaml" "$tmp/pooled.yaml" "$tmp/certs.yaml" "$tmp/nosched.yaml"; then
	pass "kubeconform"
else
	fail "kubeconform"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures assertion(s) failed" >&2
	exit 1
fi
echo "all helm assertions passed"
