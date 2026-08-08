#!/usr/bin/env bash
#
# Brings up a persistent kiln instance on Docker Desktop's Kubernetes, with the
# Claude Code CLI as the generation runner.
#
#   scripts/k8s-local.sh up          # build, load, apply, wait, print the token
#   scripts/k8s-local.sh down        # stop the workloads, keep the data
#   scripts/k8s-local.sh purge       # delete everything, including the data
#
# Usually reached through the Makefile: `make k8s-up`, `make k8s-down`, and so
# on. See deploy/local-k8s/README.md.
#
# Three properties of Docker Desktop's cluster shape everything below, and each
# one is worth knowing before changing this file:
#
#   1. It is a multi-node kind cluster whose nodes do not share the host's
#      Docker image store. A locally built image is invisible to it until it is
#      exported and imported into a node's containerd.
#   2. Its default StorageClass (local-path) binds a volume to whichever node
#      first consumed it, and the volumes here are ReadWriteOnce.
#   3. Its nodes are containers that do not see the host filesystem, so hostPath
#      cannot reach a repository on the Mac.
#
# (1) and (2) are why every kiln workload is pinned to a single node, recorded
# in a ConfigMap so restarts land where the data already is. (3) is why local
# sources are copied in with `sync` rather than mounted.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MANIFESTS="${REPO_ROOT}/deploy/local-k8s/manifests.yaml"
CLI_DOCKERFILE="${REPO_ROOT}/deploy/local-k8s/Dockerfile"

# State that must survive a `down`: which node the volumes live on, which image
# the node already has, and the bearer token minted on first boot.
STATE_DIR="${REPO_ROOT}/.dev/k8s"

NAMESPACE="${KILN_K8S_NAMESPACE:-kiln-local}"
PORT="${KILN_K8S_PORT:-8080}"
BASE_IMAGE="${KILN_K8S_BASE_IMAGE:-kiln:local}"
IMAGE="${KILN_K8S_IMAGE:-kiln-local:dev}"
CONTEXT="${KILN_K8S_CONTEXT:-docker-desktop}"

# Generation knobs, deliberately conservative: this stack spends real money on
# every build, unlike `make dev` and its fake runner.
LOG_LEVEL="${KILN_LOG_LEVEL:-info}"
AGENT_MODEL="${KILN_AGENT_MODEL:-}"
# Inherited from the shell's ANTHROPIC_BASE_URL when not set explicitly: a key
# that only works against a gateway is usually already configured that way, and
# silently ignoring it costs a confusing round of failed authentication.
AGENT_BASE_URL="${KILN_AGENT_BASE_URL:-${ANTHROPIC_BASE_URL:-}}"
RUN_BUDGET_USD="${KILN_AGENT_RUN_BUDGET_USD:-5.00}"
MAX_PAGES_PER_RUN="${KILN_AGENT_MAX_PAGES_PER_RUN:-10}"
# Above kiln's stock $0.40/$1.50. A document's extracted text rides in the
# prompt, so a single large PDF can blow through the stock analyze ceiling on
# its first call -- which reads as "budget_exhausted" after real spend, not as
# a limit worth raising. Both stay under RUN_BUDGET_USD: the ledger reserves a
# call's budget before making it, so a per-call ceiling above the run ceiling
# can never be reserved and every unit would die on its first call.
ANALYZE_BUDGET_USD="${KILN_AGENT_ANALYZE_BUDGET_USD:-1.00}"
PAGE_BUDGET_USD="${KILN_AGENT_PAGE_BUDGET_USD:-2.00}"

STATE_CM="kiln-local-state"

# --- output ------------------------------------------------------------------

if [ -t 1 ]; then
	BOLD=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; YELLOW=$'\033[33m'; RESET=$'\033[0m'
else
	BOLD=""; DIM=""; RED=""; YELLOW=""; RESET=""
fi

# Progress goes to stderr, not stdout: several helpers below return a value by
# printing it, and a stray "==> building" inside a command substitution ends up
# spliced into a manifest.
say()  { printf '%s==>%s %s\n' "$BOLD" "$RESET" "$*" >&2; }
note() { printf '%s    %s%s\n' "$DIM" "$*" "$RESET" >&2; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

k() { kubectl --context "$CONTEXT" "$@"; }
kn() { kubectl --context "$CONTEXT" --namespace "$NAMESPACE" "$@"; }

# --- preflight ---------------------------------------------------------------

preflight() {
	command -v docker  >/dev/null 2>&1 || die "docker not found. Install Docker Desktop."
	command -v kubectl >/dev/null 2>&1 || die "kubectl not found. Docker Desktop ships one; enable Kubernetes in its settings."

	docker info >/dev/null 2>&1 || die "the Docker daemon is not responding. Start Docker Desktop."

	if ! k config get-contexts "$CONTEXT" >/dev/null 2>&1; then
		die "no kubectl context named '$CONTEXT'.
    Enable Kubernetes in Docker Desktop (Settings -> Kubernetes -> Enable Kubernetes),
    or point this script at another cluster with KILN_K8S_CONTEXT=<name>."
	fi

	k cluster-info >/dev/null 2>&1 || die "context '$CONTEXT' exists but the cluster is unreachable. Is Kubernetes still starting?"

	if [ -z "$(k get storageclass -o name 2>/dev/null)" ]; then
		die "the cluster has no StorageClass, so nothing here can persist.
    Docker Desktop normally provides 'standard'; a reset of the Kubernetes cluster restores it."
	fi
}

# Ready, uncordoned node names, one per line. A go-template rather than
# jsonpath: `.spec.unschedulable` is absent rather than false on a healthy node,
# and a jsonpath filter on an absent field matches nothing at all.
schedulable_nodes() {
	k get nodes "$@" -o go-template='
		{{- range .items -}}
			{{- if not .spec.unschedulable -}}
				{{- $name := .metadata.name -}}
				{{- range .status.conditions -}}
					{{- if and (eq .type "Ready") (eq .status "True") -}}
						{{- $name }}{{ "\n" -}}
					{{- end -}}
				{{- end -}}
			{{- end -}}
		{{- end -}}'
}

# The node every kiln workload is pinned to. Sticky: once volumes are bound to
# a node, moving the pods elsewhere would leave them Pending on a volume they
# cannot reach, so the choice is recorded and reused.
resolve_node() {
	local node
	node="$(kn get configmap "$STATE_CM" -o jsonpath='{.data.node}' 2>/dev/null || true)"

	if [ -n "$node" ]; then
		if ! k get node "$node" >/dev/null 2>&1; then
			die "this stack's data lives on node '$node', which no longer exists.
    Resetting the Kubernetes cluster deletes the volumes too, so the honest fix is:
      make k8s-purge && make k8s-up"
		fi
		printf '%s' "$node"
		return
	fi

	# Prefer a worker: on a multi-node cluster the control plane usually carries
	# a NoSchedule taint, and pinning to it would strand every pod.
	node="$(schedulable_nodes -l '!node-role.kubernetes.io/control-plane' | head -1)"

	# Single-node clusters (Docker Desktop's older Kubernetes, minikube) have
	# only a control plane, and it is schedulable.
	[ -n "$node" ] || node="$(schedulable_nodes | head -1)"

	[ -n "$node" ] || die "no schedulable node in context '$CONTEXT'."
	printf '%s' "$node"
}

# --- secrets -----------------------------------------------------------------

rand_hex() { openssl rand -hex 32; }

secret_value() {
	local key="$1" encoded
	encoded="$(kn get secret kiln-secrets -o jsonpath="{.data.${key}}" 2>/dev/null || true)"
	[ -n "$encoded" ] || return 0
	printf '%s' "$encoded" | base64 -d
}

# Generated once and reused forever after. Regenerating the master key on every
# `up` would silently orphan every connector credential already sealed with the
# old one, which surfaces much later as an undecryptable secret.
ensure_secrets() {
	local master pgpass api_key

	master="$(secret_value master-key)"
	pgpass="$(secret_value postgres-password)"

	if [ -z "$master" ]; then
		say "minting a master key"
		master="$(rand_hex)"
		note "seals connector credentials at rest; kept in the kiln-secrets Secret"
	fi
	# Hex, so it needs no percent-encoding inside the connection URL.
	[ -n "$pgpass" ] || pgpass="$(rand_hex)"

	# Taken fresh from the environment each run so rotating a key is just
	# `ANTHROPIC_API_KEY=... make k8s-up`. An unset variable keeps whatever is
	# already in the cluster rather than blanking it.
	api_key="${ANTHROPIC_API_KEY:-${KILN_ANTHROPIC_API_KEY:-}}"
	[ -n "$api_key" ] || api_key="$(secret_value anthropic-api-key)"

	kn create secret generic kiln-secrets \
		--from-literal=master-key="$master" \
		--from-literal=postgres-password="$pgpass" \
		--from-literal=database-url="postgres://kiln:${pgpass}@postgres:5432/kiln?sslmode=disable" \
		--from-literal=anthropic-api-key="$api_key" \
		--dry-run=client -o yaml | kn apply -f - >/dev/null

	# Optional second path to model access: a Claude Code session credential,
	# for driving the CLI on a subscription rather than an API key. Never read
	# from a default location -- the operator has to point at the file.
	if [ -n "${KILN_CLAUDE_CREDENTIALS_FILE:-}" ]; then
		[ -f "$KILN_CLAUDE_CREDENTIALS_FILE" ] || die "KILN_CLAUDE_CREDENTIALS_FILE is set but $KILN_CLAUDE_CREDENTIALS_FILE does not exist"
		say "loading Claude Code credentials from $KILN_CLAUDE_CREDENTIALS_FILE"
		kn create secret generic kiln-claude-credentials \
			--from-file=credentials.json="$KILN_CLAUDE_CREDENTIALS_FILE" \
			--dry-run=client -o yaml | kn apply -f - >/dev/null
	fi

	if [ -z "$api_key" ] && ! kn get secret kiln-claude-credentials >/dev/null 2>&1; then
		warn "no model credentials configured -- builds will fail at the first page
    with 'Not logged in'. Set one before building:

      ANTHROPIC_API_KEY=sk-ant-... make k8s-up

    A Claude Code session credential works too, but it has to be a file:
    KILN_CLAUDE_CREDENTIALS_FILE=/path/to/.credentials.json. On macOS the CLI
    keeps it in the Keychain, so there is no such file until you export one --
    see deploy/local-k8s/README.md."
	fi
}

# Fingerprints the secrets the pods consume, so that changing one is a pod spec
# change and rolls them. Hashed rather than embedded: the annotation is readable
# by anything that can read the pod.
secret_hash() {
	{
		kn get secret kiln-secrets -o jsonpath='{.data}' 2>/dev/null || true
		kn get secret kiln-claude-credentials -o jsonpath='{.data}' 2>/dev/null || true
	} | shasum -a 256 | cut -d' ' -f1
}

# --- images ------------------------------------------------------------------

build_images() {
	say "building $BASE_IMAGE"
	docker build -q -t "$BASE_IMAGE" --build-arg VERSION=k8s-local "$REPO_ROOT" >/dev/null

	say "building $IMAGE (adds the Claude Code CLI)"
	docker build -q -t "$IMAGE" \
		--build-arg BASE_IMAGE="$BASE_IMAGE" \
		${KILN_CLAUDE_CODE_VERSION:+--build-arg CLAUDE_CODE_VERSION="$KILN_CLAUDE_CODE_VERSION"} \
		-f "$CLI_DOCKERFILE" "$REPO_ROOT" >/dev/null
}

# The cluster's nodes run their own containerd and cannot see the host's image
# store, so the image is exported and imported rather than pulled. Skipped when
# the node already holds this exact image: the export is most of a gigabyte.
load_image() {
	local node="$1" image_id loaded tar
	image_id="$(docker image inspect "$IMAGE" --format '{{.Id}}')"
	loaded="$(kn get configmap "$STATE_CM" -o jsonpath='{.data.image-id}' 2>/dev/null || true)"

	if [ "$image_id" = "$loaded" ]; then
		note "node $node already has this image"
		printf '%s' "$image_id"
		return
	fi

	docker exec "$node" true >/dev/null 2>&1 || die "cannot reach node '$node' as a container.
    This script loads images with 'docker exec $node ctr ...', which needs the cluster's
    nodes to be containers on this Docker daemon -- true for Docker Desktop and kind.
    On another cluster, push $IMAGE to a registry the cluster can pull from instead."

	say "loading $IMAGE into node $node"
	tar="$(mktemp -t kiln-k8s-image)"
	# shellcheck disable=SC2064  # expand $tar now, at trap definition
	trap "rm -f '$tar'" RETURN
	docker save "$IMAGE" -o "$tar"
	docker exec -i "$node" ctr -n k8s.io images import - < "$tar" >/dev/null
	note "$(du -h "$tar" | cut -f1) imported"

	printf '%s' "$image_id"
}

# --- apply -------------------------------------------------------------------

render() {
	local node="$1" image_id="$2"
	sed \
		-e "s|__NAMESPACE__|${NAMESPACE}|g" \
		-e "s|__NODE__|${node}|g" \
		-e "s|__IMAGE__|${IMAGE}|g" \
		-e "s|__IMAGE_ID__|${image_id}|g" \
		-e "s|__SECRET_HASH__|$(secret_hash)|g" \
		-e "s|__PORT__|${PORT}|g" \
		-e "s|__LOG_LEVEL__|${LOG_LEVEL}|g" \
		-e "s|__MODEL__|${AGENT_MODEL}|g" \
		-e "s|__BASE_URL__|${AGENT_BASE_URL}|g" \
		-e "s|__RUN_BUDGET_USD__|${RUN_BUDGET_USD}|g" \
		-e "s|__MAX_PAGES_PER_RUN__|${MAX_PAGES_PER_RUN}|g" \
		-e "s|__ANALYZE_BUDGET_USD__|${ANALYZE_BUDGET_USD}|g" \
		-e "s|__PAGE_BUDGET_USD__|${PAGE_BUDGET_USD}|g" \
		"$MANIFESTS"
}

# The manifest file is one document set split by a sentinel comment into what
# must exist before kiln starts (database, schema, volumes) and what depends on
# it. Applying both at once works, but the API and the worker exit on a schema
# version they do not recognise, so it works by way of a CrashLoopBackOff.
render_stage() {
	local stage="$1" node="$2" image_id="$3"
	render "$node" "$image_id" | awk -v want="$stage" '
		BEGIN { s = "infra" }
		/^# @stage workloads$/ { s = "workloads"; next }
		s == want
	'
}

save_state() {
	local node="$1" image_id="$2"
	kn create configmap "$STATE_CM" \
		--from-literal=node="$node" \
		--from-literal=image-id="$image_id" \
		--dry-run=client -o yaml | kn apply -f - >/dev/null
}

mint_token() {
	local pod token
	pod="$(kn get pod -l app.kubernetes.io/component=api -o jsonpath='{.items[0].metadata.name}')"
	# Printed exactly once and only its hash stored, so it is captured here
	# rather than re-derived later. --admin because the operator of a laptop
	# instance is also its administrator.
	token="$(kn exec "$pod" -c api -- \
		kiln admin token create --login local --scopes read,write,admin --admin 2>/dev/null | tail -1)"
	[ -n "$token" ] || die "could not mint a token; try 'make k8s-logs'"

	mkdir -p "$STATE_DIR"
	umask 077
	printf '%s\n' "$token" > "${STATE_DIR}/token"
	printf '%s' "$token"
}

# --- subcommands -------------------------------------------------------------

cmd_up() {
	preflight

	k create namespace "$NAMESPACE" --dry-run=client -o yaml | k apply -f - >/dev/null

	local node image_id
	node="$(resolve_node)"
	say "pinning to node $node"

	ensure_secrets
	build_images
	image_id="$(load_image "$node")"

	# Recreated rather than patched: a Job's pod template is immutable, so an
	# apply over a previous run's Job is rejected outright.
	kn delete job kiln-migrate --ignore-not-found --wait=true >/dev/null 2>&1 || true

	say "applying database and volumes"
	render_stage infra "$node" "$image_id" | kn apply -f - >/dev/null
	save_state "$node" "$image_id"

	say "waiting for postgres"
	kn rollout status statefulset/postgres --timeout=5m

	say "running migrations"
	if ! kn wait --for=condition=complete job/kiln-migrate --timeout=5m >/dev/null; then
		kn logs job/kiln-migrate --tail=40 || true
		die "migrations did not complete"
	fi

	say "applying the API and worker"
	render_stage workloads "$node" "$image_id" | kn apply -f - >/dev/null

	say "waiting for the API and worker"
	kn rollout status deployment/kiln-api --timeout=5m
	kn rollout status deployment/kiln-worker --timeout=5m

	local token
	if [ -s "${STATE_DIR}/token" ]; then
		token="$(cat "${STATE_DIR}/token")"
		note "reusing the token in .dev/k8s/token"
	else
		say "minting an API token"
		token="$(mint_token)"
	fi

	cat <<-EOF

	  ${BOLD}kiln is up${RESET} on http://localhost:${PORT}

	  token   ${token}
	          saved to .dev/k8s/token; paste it when the UI asks

	  Generation runs through the Claude Code CLI (agent.runner = "cli") in the
	  worker pod. Postgres and the blob store are on PersistentVolumeClaims, so
	  'make k8s-down' stops the workloads without losing the wiki.

	  ${DIM}make k8s-status         what is running
	  make k8s-logs           follow the worker
	  make k8s-sync SRC=path  copy a local repository in, to ingest as /sources/<name>
	  make k8s-down           stop, keeping data
	  make k8s-purge          delete everything, data included${RESET}

	EOF
}

cmd_down() {
	preflight
	kn get namespace >/dev/null 2>&1 || { note "nothing to stop"; return; }
	say "stopping workloads in $NAMESPACE"
	# PersistentVolumeClaims, Secrets and the ConfigMaps are left alone: this is
	# the "stop for the night" verb. `purge` is the destructive one.
	kn delete deployment,statefulset,job --all --wait=false >/dev/null
	note "data kept -- 'make k8s-up' brings it back, 'make k8s-purge' deletes it"
}

cmd_purge() {
	preflight
	kn get namespace >/dev/null 2>&1 || { note "nothing to delete"; return; }
	say "deleting namespace $NAMESPACE, including all volumes"
	k delete namespace "$NAMESPACE" --wait=true
	rm -f "${STATE_DIR}/token"
	note "purged"
}

cmd_status() {
	preflight
	kn get namespace >/dev/null 2>&1 || { note "$NAMESPACE does not exist; run 'make k8s-up'"; return; }
	kn get pods,svc,pvc
	printf '\n'
	note "UI: http://localhost:${PORT}"
}

cmd_logs() {
	preflight
	local component="${1:-worker}"
	kn logs -f -l "app.kubernetes.io/component=${component}" --tail=100 --max-log-requests=5
}

cmd_token() {
	preflight
	say "minting a new API token"
	mint_token
	printf '\n'
}

# Local repositories cannot be mounted: the cluster's nodes are containers with
# no view of the host filesystem. Copying into the sources volume is the way
# in, and /sources is the only path the worker is permitted to read from.
cmd_sync() {
	preflight
	local src="${1:-}"
	[ -n "$src" ] || die "usage: scripts/k8s-local.sh sync <path>   (or: make k8s-sync SRC=<path>)"
	[ -d "$src" ] || die "$src is not a directory"

	local pod name
	name="$(basename "$(cd "$src" && pwd)")"
	pod="$(kn get pod -l app.kubernetes.io/component=worker -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	[ -n "$pod" ] || die "no worker pod is running; 'make k8s-up' first"

	say "copying $src to /sources/$name"
	kn cp "$src" "${pod}:/sources/${name}" -c worker

	cat <<-EOF

	  Copied. Point a repository connector at ${BOLD}/sources/${name}${RESET} to ingest it.

	  ${DIM}The copy is a snapshot: re-run this after the source changes, then rebuild.${RESET}

	EOF
}

# Clears the stored Claude Code credential so the next start re-seeds from the
# secret. Needed because the worker's HOME persists and is never overwritten:
# that is what lets the CLI's token refresh survive a restart, and it is also
# what makes a freshly exported credential otherwise get ignored.
cmd_reauth() {
	preflight
	local pod
	pod="$(kn get pod -l app.kubernetes.io/component=worker -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	if [ -n "$pod" ]; then
		kn exec "$pod" -c worker -- rm -f /home/kiln/.claude/.credentials.json 2>/dev/null || true
	fi
	say "cleared the stored credential; restarting the worker"
	kn rollout restart deployment/kiln-worker >/dev/null
	kn rollout status deployment/kiln-worker --timeout=5m
	note "re-seeded from kiln-claude-credentials -- export a fresh one and run k8s-up first if it was stale"
}

cmd_shell() {
	preflight
	local pod
	pod="$(kn get pod -l app.kubernetes.io/component=worker -o jsonpath='{.items[0].metadata.name}')"
	kn exec -it "$pod" -c worker -- bash
}

# pod_of prints a running pod for a component, or explains what to do instead.
# It reports failure rather than calling die: callers read it through command
# substitution, and a die there would exit only the subshell -- leaving the
# caller to run kubectl with an empty pod name and print a second, confusing
# error underneath the useful one.
pod_of() {
	local component="$1" pod
	pod="$(kn get pod -l app.kubernetes.io/component="$component" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	if [ -z "$pod" ]; then
		printf '%serror:%s no %s pod is running; run "make k8s-up" first\n' \
			"$RED" "$RESET" "$component" >&2
		return 1
	fi
	printf '%s' "$pod"
}

# doctor and migrate exist locally as `make doctor` and `make migrate`, and the
# questions they answer -- is the configuration sound, is the schema current --
# are worth more against the cluster than against a laptop, because that is
# where the answer is not obvious. Run inside the pod rather than from the host:
# the configuration lives in the pod's environment and the database is only
# reachable from inside the namespace.
#
# The worker is the pod to ask: it is the one that runs the agent, so it is the
# only one whose CLI credential state doctor can see.
cmd_doctor() {
	preflight
	local pod
	pod="$(pod_of worker)" || return 1
	kn exec "$pod" -c worker -- kiln admin doctor "$@"
}

# k8s-up runs migrations as a Job before the deployments start, so this is for
# the case that Job cannot cover: a schema change applied to a cluster that is
# already up, without a full redeploy.
cmd_migrate() {
	preflight
	local pod
	pod="$(pod_of api)" || return 1
	kn exec "$pod" -c api -- kiln admin migrate
}

usage() {
	cat <<-EOF
	usage: scripts/k8s-local.sh <command>

	  up               build, load, apply, and wait; prints the URL and token
	  down             stop the workloads, keep the data
	  purge            delete the namespace and every volume in it
	  status           pods, services, and volumes
	  logs [component] follow logs (api, worker, postgres; default worker)
	  token            mint another API token
	  sync <path>      copy a local directory into the sources volume
	  reauth           replace the stored Claude Code credential with the secret's
	  shell            a shell in the worker pod
	  doctor [--probe] check config, database, schema, and the CLI credential
	  migrate          apply pending migrations to a cluster already running

	environment:
	  ANTHROPIC_API_KEY              model access for the Claude Code CLI
	  KILN_CLAUDE_CREDENTIALS_FILE   a Claude Code session credential, instead of a key
	  KILN_K8S_PORT                  host port for the UI (default 8080)
	  KILN_K8S_NAMESPACE             namespace (default kiln-local)
	  KILN_K8S_CONTEXT               kubectl context (default docker-desktop)
	  KILN_AGENT_MODEL               model override, e.g. claude-sonnet-5
	  KILN_AGENT_BASE_URL            gateway/proxy endpoint for the CLI;
	                                 defaults to the shell's ANTHROPIC_BASE_URL
	  KILN_AGENT_RUN_BUDGET_USD      per-run spend ceiling (default 5.00)
	  KILN_AGENT_MAX_PAGES_PER_RUN   pages per run (default 10)
	  KILN_AGENT_ANALYZE_BUDGET_USD  per-analyze-call ceiling (default 1.00)
	  KILN_AGENT_PAGE_BUDGET_USD     per-page-call ceiling (default 2.00)
	EOF
}

main() {
	local cmd="${1:-up}"
	shift || true
	case "$cmd" in
		up)     cmd_up "$@" ;;
		down)   cmd_down "$@" ;;
		purge)  cmd_purge "$@" ;;
		status) cmd_status "$@" ;;
		logs)   cmd_logs "$@" ;;
		token)  cmd_token "$@" ;;
		sync)   cmd_sync "$@" ;;
		reauth) cmd_reauth "$@" ;;
		shell)  cmd_shell "$@" ;;
		doctor) cmd_doctor "$@" ;;
		migrate) cmd_migrate "$@" ;;
		-h|--help|help) usage ;;
		*)      usage >&2; exit 2 ;;
	esac
}

main "$@"
