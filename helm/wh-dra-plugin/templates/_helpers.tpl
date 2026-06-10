{{/*
Full image reference, e.g. ghcr.io/org/wh-dra-plugin:sha-abc1234
*/}}
{{- define "wh-dra-plugin.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "wh-dra-plugin.labels" -}}
app.kubernetes.io/name: wh-dra-plugin
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
{{- end }}
