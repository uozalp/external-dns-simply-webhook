{{/*
Expand the name of the chart.
*/}}
{{- define "external-dns-simply.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "external-dns-simply.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else if contains .Release.Name $name }}
{{- $name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "external-dns-simply.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "external-dns-simply.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for the webhook
*/}}
{{- define "external-dns-simply.selectorLabels" -}}
app.kubernetes.io/name: {{ include "external-dns-simply.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Webhook component name
*/}}
{{- define "external-dns-simply.webhook.fullname" -}}
{{- printf "%s-webhook" (include "external-dns-simply.fullname" .) }}
{{- end }}

{{/*
Selector labels for external-dns
*/}}
{{- define "external-dns-simply.externalDns.selectorLabels" -}}
app.kubernetes.io/name: {{ include "external-dns-simply.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: external-dns
{{- end }}
