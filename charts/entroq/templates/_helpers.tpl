{{/*
Common labels applied to every resource in this chart.
*/}}
{{- define "entroq.labels" -}}
app.kubernetes.io/name: entroq
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
Selector labels for the EntroQ deployment/statefulset.
*/}}
{{- define "entroq.selectorLabels" -}}
app.kubernetes.io/name: entroq
app.kubernetes.io/component: server
{{- end }}

{{/*
Selector labels for the operator deployment.
*/}}
{{- define "entroq.operatorSelectorLabels" -}}
app.kubernetes.io/name: entroq
app.kubernetes.io/component: operator
{{- end }}

{{/*
Secret name helpers. Use existingSecret if provided; otherwise the chart
creates a Secret named <release>-postgres or <release>-redis.
*/}}
{{- define "entroq.postgresSecretName" -}}
{{- if .Values.entroq.postgres.existingSecret -}}
{{ .Values.entroq.postgres.existingSecret }}
{{- else -}}
{{ .Release.Name }}-postgres
{{- end -}}
{{- end }}

{{- define "entroq.redisSecretName" -}}
{{- if .Values.entroq.redis.existingSecret -}}
{{ .Values.entroq.redis.existingSecret }}
{{- else -}}
{{ .Release.Name }}-redis
{{- end -}}
{{- end }}

{{/*
entroq.serverImage returns the full image reference for the EntroQ server
container based on backend.type. Tag defaults to .Chart.AppVersion.
*/}}
{{- define "entroq.serverImage" -}}
{{- $type := .Values.entroq.backend.type -}}
{{- $img := .Values.entroq.images.mem -}}
{{- if eq $type "postgres" -}}{{- $img = .Values.entroq.images.postgres -}}{{- end -}}
{{- if eq $type "redis"    -}}{{- $img = .Values.entroq.images.redis    -}}{{- end -}}
{{- $tag := $img.tag | default .Chart.AppVersion -}}
{{- printf "%s:%s" $img.repository $tag -}}
{{- end }}

{{/*
OPA extra args derived from values. Appended to the base opa run args.
*/}}
{{- define "entroq.opaExtraArgs" -}}
{{- if .Values.entroq.opa.decisionLogs }}
- --set=decision_logs.console=true
{{- end }}
{{- if .Values.entroq.opa.debug }}
- --log-level=debug
{{- end }}
{{- end }}
