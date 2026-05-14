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
