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
{{- with .Values.config.publicURL }}
- name: KILN_PUBLIC_URL
  value: {{ . | quote }}
{{- end }}
{{- with .Values.config.corsOrigins }}
- name: KILN_CORS_ORIGINS
  value: {{ join "," . | quote }}
{{- end }}
{{- with .Values.config.database.host }}
- name: KILN_DATABASE_HOST
  value: {{ . | quote }}
- name: KILN_DATABASE_PORT
  value: {{ $.Values.config.database.port | quote }}
- name: KILN_DATABASE_NAME
  value: {{ $.Values.config.database.name | quote }}
- name: KILN_DATABASE_USER
  value: {{ $.Values.config.database.user | quote }}
- name: KILN_DATABASE_SSLMODE
  value: {{ $.Values.config.database.sslmode | quote }}
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
{{- with .Values.config.github.appSlug }}
- name: KILN_GITHUB_APP_SLUG
  value: {{ . | quote }}
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
- name: KILN_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: database-url
      optional: true
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
