{{/* Chart name, overridable. */}}
{{- define "kiln.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kiln.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kiln.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kiln.labels" -}}
helm.sh/chart: {{ include "kiln.chart" . }}
{{ include "kiln.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kiln.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kiln.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kiln.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kiln.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Pin by digest when given: a tag is a moving target, and an enterprise
     rollout should be able to say exactly what is running. */}}
{{- define "kiln.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "kiln.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "kiln.fullname" . -}}
{{- end -}}
{{- end -}}

{{/* Non-secret configuration, shared by every role so the API and its workers
     can never disagree about what they are pointed at. */}}
{{- define "kiln.env" -}}
- name: KILN_LOG_LEVEL
  value: {{ .Values.config.logLevel | quote }}
- name: KILN_AUTH_MODE
  value: {{ .Values.config.authMode | quote }}
{{- with .Values.config.authDefaultRole }}
- name: KILN_AUTH_DEFAULT_ROLE
  value: {{ . | quote }}
{{- end }}
- name: KILN_METRICS_ADDR
  value: {{ .Values.config.metricsAddr | quote }}
{{- with .Values.config.publicURL }}
- name: KILN_PUBLIC_URL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.config.corsOrigins }}
- name: KILN_CORS_ORIGINS
  value: {{ join "," . | quote }}
{{- end }}
{{- with .Values.config.trustedProxies }}
- name: KILN_TRUSTED_PROXIES
  value: {{ join "," . | quote }}
{{- end }}
- name: KILN_STORAGE_BACKEND
  value: {{ .Values.config.storage.backend | quote }}
{{- if eq .Values.config.storage.backend "s3" }}
- name: KILN_STORAGE_BUCKET
  value: {{ .Values.config.storage.bucket | quote }}
- name: KILN_STORAGE_REGION
  value: {{ .Values.config.storage.region | quote }}
- name: KILN_STORAGE_USE_SSL
  value: {{ .Values.config.storage.useSSL | quote }}
- name: KILN_STORAGE_PATH_STYLE
  value: {{ .Values.config.storage.pathStyle | quote }}
{{- with .Values.config.storage.endpoint }}
- name: KILN_STORAGE_ENDPOINT
  value: {{ . | quote }}
{{- end }}
{{- else }}
- name: KILN_STORAGE_PATH
  value: {{ .Values.config.storage.fsPath | quote }}
{{- end }}
- name: KILN_AGENT_RUNNER
  value: {{ .Values.config.agent.runner | quote }}
{{- with .Values.config.agent.model }}
- name: KILN_AGENT_MODEL
  value: {{ . | quote }}
{{- end }}
- name: KILN_AGENT_RUN_BUDGET_USD
  value: {{ .Values.config.agent.runBudgetUSD | quote }}
- name: KILN_AGENT_MAX_PAGES_PER_RUN
  value: {{ .Values.config.agent.maxPagesPerRun | quote }}
- name: KILN_AGENT_BUDGET_WINDOW
  value: {{ .Values.config.agent.budgetWindow | quote }}
{{- with .Values.config.github.clientID }}
- name: KILN_GITHUB_CLIENT_ID
  value: {{ . | quote }}
{{- end }}
{{- with .Values.config.github.appID }}
- name: KILN_GITHUB_APP_ID
  value: {{ . | quote }}
{{- end }}
{{- end -}}

{{/* Where a role connects to Postgres. Called with (dict "root" $ "direct" bool).
     With the pooler enabled the API and workers go through it, with an exec
     mode that leaves no named prepared statement on a server connection; the
     migrate Job passes direct=true and keeps connecting to Postgres itself,
     because it runs as a pre-install hook before the pooler exists. */}}
{{- define "kiln.databaseEnv" -}}
{{- $db := .root.Values.config.database -}}
{{- if and .root.Values.pooler.enabled (not .direct) }}
- name: KILN_DATABASE_HOST
  value: {{ include "kiln.poolerName" .root | quote }}
- name: KILN_DATABASE_PORT
  value: "6432"
- name: KILN_DATABASE_NAME
  value: {{ $db.name | quote }}
- name: KILN_DATABASE_USER
  value: {{ $db.user | quote }}
- name: KILN_DATABASE_SSLMODE
  value: {{ $db.sslmode | quote }}
- name: KILN_DATABASE_QUERY_EXEC_MODE
  value: "cache_describe"
{{- else if $db.host }}
- name: KILN_DATABASE_HOST
  value: {{ $db.host | quote }}
- name: KILN_DATABASE_PORT
  value: {{ $db.port | quote }}
- name: KILN_DATABASE_NAME
  value: {{ $db.name | quote }}
- name: KILN_DATABASE_USER
  value: {{ $db.user | quote }}
- name: KILN_DATABASE_SSLMODE
  value: {{ $db.sslmode | quote }}
{{- end }}
{{- end -}}

{{/* Secrets are referenced, never inlined into the pod spec: optional: true
     everywhere so a deployment that does not use GitHub sign-in needs no
     placeholder keys. */}}
{{- define "kiln.secretEnv" -}}
{{- $secret := include "kiln.secretName" . -}}
- name: KILN_MASTER_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: master-key
- name: KILN_ANTHROPIC_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: anthropic-api-key
      optional: true
{{- /* A URL wins over the discrete fields, so with the pooler enabled it is
       not referenced at all: a database-url key would otherwise send every
       pod straight past the pooler without a word. */}}
{{- if not .Values.pooler.enabled }}
- name: KILN_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: database-url
      optional: true
{{- end }}
- name: KILN_DATABASE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: database-password
      optional: true
- name: KILN_STORAGE_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: storage-secret-key
      optional: true
- name: KILN_STORAGE_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: storage-access-key
      optional: true
- name: KILN_GITHUB_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: github-client-secret
      optional: true
- name: KILN_GITHUB_PRIVATE_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: github-private-key
      optional: true
- name: KILN_GITHUB_WEBHOOK_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: github-webhook-secret
      optional: true
{{- end -}}

{{/* readOnlyRootFilesystem means every writable path must be declared. The
     worker stages clones and extracted documents under TMPDIR. */}}
{{- define "kiln.volumes" -}}
- name: tmp
  emptyDir: {}
{{- end -}}

{{- define "kiln.volumeMounts" -}}
- name: tmp
  mountPath: /tmp
{{- end -}}


{{/* --- connection pooler --------------------------------------------------- */}}

{{- define "kiln.poolerName" -}}
{{- printf "%s-pooler" (include "kiln.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kiln.poolerImage" -}}
{{- $img := .Values.pooler.image -}}
{{- if $img.digest -}}
{{- printf "%s:%s@%s" $img.repository $img.tag $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository $img.tag -}}
{{- end -}}
{{- end -}}

{{- define "kiln.poolerTLSSecretName" -}}
{{- default (printf "%s-tls" (include "kiln.poolerName" .)) .Values.pooler.tls.secretName -}}
{{- end -}}

{{/* pgbouncer.ini. The same settings as deploy/pgbouncer/pgbouncer.ini, which
     compose and CI run; scripts/helm-assert.sh checks the security-relevant
     keys agree. No credential: the userlist is built at pod start from the
     database-password secret key. */}}
{{- define "kiln.poolerIni" -}}
{{- $db := .Values.config.database -}}
{{- $p := .Values.pooler -}}
[databases]
{{ $db.name }} = host={{ $db.host }} port={{ $db.port }} dbname={{ $db.name }}

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = scram-sha-256
auth_file = /tmp/pgbouncer/userlist.txt
pool_mode = transaction
max_prepared_statements = 200
ignore_startup_parameters = extra_float_digits
default_pool_size = {{ int $p.defaultPoolSize }}
reserve_pool_size = {{ int $p.reservePoolSize }}
max_client_conn = {{ int $p.maxClientConn }}
query_wait_timeout = 30
max_packet_size = 67108864
client_login_timeout = 10
; kiln to PgBouncer: TLS whenever kiln is configured to send it.
client_tls_sslmode = {{ ternary "prefer" "require" (eq $db.sslmode "disable") }}
client_tls_cert_file = /etc/pgbouncer-tls/tls.crt
client_tls_key_file = /etc/pgbouncer-tls/tls.key
server_tls_sslmode = {{ $p.serverTlsSslmode }}
{{- if $p.serverTLS.caSecretName }}
server_tls_ca_file = /etc/pgbouncer-server-ca/ca.crt
{{- end }}
{{- end -}}

{{/* Holds the API and workers until the pooler answers, rather than letting
     them crash-loop through their own connect retries on a fresh install.
     Bounded, so a pooler that never comes up fails the pod visibly. */}}
{{- define "kiln.poolerWaitInit" -}}
- name: wait-for-pooler
  image: {{ include "kiln.poolerImage" . }}
  imagePullPolicy: {{ .Values.pooler.image.pullPolicy }}
  command:
    - /bin/sh
    - -c
    - >-
      for i in $(seq 1 60); do
      pg_isready -q -h {{ include "kiln.poolerName" . }} -p 6432 && exit 0;
      sleep 2;
      done;
      echo "pooler {{ include "kiln.poolerName" . }}:6432 not ready after 120s" >&2;
      exit 1
  securityContext:
    {{- toYaml .Values.securityContext | nindent 4 }}
  resources:
    requests:
      cpu: 10m
      memory: 16Mi
    limits:
      memory: 64Mi
{{- end -}}


{{/* --- scheduler duties ----------------------------------------------------- */}}

{{/* The settings the duties read, shared by the workers and the scheduler so
     a deployment that moves the duties between them keeps the same stale
     deadline and poll period. The worker also derives its heartbeat from
     stale_after. */}}
{{- define "kiln.dutyTimingEnv" -}}
- name: KILN_WORKER_STALE_AFTER
  value: {{ .Values.config.worker.staleAfter | quote }}
- name: KILN_WORKER_SOURCE_POLL_INTERVAL
  value: {{ .Values.config.worker.sourcePollInterval | quote }}
{{- end -}}

{{/* worker.schedulerDuties resolved to "true" or "false": auto follows
     scheduler.enabled. Anything else fails the render rather than being
     guessed at. */}}
{{- define "kiln.workerSchedulerDuties" -}}
{{- $v := toString .Values.worker.schedulerDuties -}}
{{- if eq $v "auto" -}}
{{- toString (not .Values.scheduler.enabled) -}}
{{- else if or (eq $v "true") (eq $v "false") -}}
{{- $v -}}
{{- else -}}
{{- fail (printf "worker.schedulerDuties must be auto, true or false, got %q" $v) -}}
{{- end -}}
{{- end -}}

{{/* The scheduler's secrets: the database and object storage, nothing else.
     It never opens a credential or calls a model, so the master key, the
     model key and the GitHub secrets stay out of its pod. */}}
{{- define "kiln.schedulerSecretEnv" -}}
{{- $secret := include "kiln.secretName" . -}}
- name: KILN_DATABASE_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: database-password
      optional: true
{{- /* As in kiln.secretEnv: a URL would bypass the pooler. */}}
{{- if not .Values.pooler.enabled }}
- name: KILN_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: database-url
      optional: true
{{- end }}
- name: KILN_STORAGE_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: storage-secret-key
      optional: true
- name: KILN_STORAGE_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: storage-access-key
      optional: true
{{- end -}}
