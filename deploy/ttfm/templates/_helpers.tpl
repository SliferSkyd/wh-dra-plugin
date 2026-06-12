{{/*
Expand the name of the chart.
*/}}
{{- define "ttfm.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "ttfm.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "ttfm.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "ttfm.labels" -}}
helm.sh/chart: {{ include "ttfm.chart" . }}
{{ include "ttfm.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "ttfm.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ttfm.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Volume source for the scaleout configs (FSDs), mounted read-only at
/scaleout_configs in both the controller and agent pods.

Defaults to a hostPath on the node. When `scaleoutConfigsImage` is set the
volume is sourced from an OCI image instead (requires the Kubernetes
ImageVolume feature, beta/on-by-default since 1.33). Image pulls reuse the
pod's imagePullSecrets (attached via the service account).
*/}}
{{- define "ttfm.scaleoutConfigsVolume" -}}
- name: scaleout-configs
{{- if .Values.scaleoutConfigsImage }}
  image:
    reference: {{ .Values.scaleoutConfigsImage | quote }}
    pullPolicy: {{ .Values.image.pullPolicy | default "IfNotPresent" }}
{{- else }}
  hostPath:
    path: {{ .Values.scaleoutConfigsHostPath }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "ttfm.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ttfm.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
