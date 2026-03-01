{{- define "ai-platform.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ai-platform.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "ai-platform.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "ai-platform.component.fullname" -}}
{{- $root := index . "root" -}}
{{- $component := index . "component" -}}
{{- printf "%s-%s" (include "ai-platform.fullname" $root) $component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ai-platform.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "ai-platform.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "ai-platform.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ai-platform.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "ai-platform.image" -}}
{{- $root := index . "root" -}}
{{- $image := index . "image" -}}
{{- printf "%s%s:%s" (default "" $root.Values.global.imageRegistry) $image.repository $image.tag -}}
{{- end -}}

{{- define "ai-platform.dbUrl" -}}
{{- if .Values.secrets.databaseUrl -}}
{{ .Values.secrets.databaseUrl | quote }}
{{- else -}}
{{ printf "postgres://postgres:postgres@%s-postgresql:5432/agentdb?sslmode=disable" .Release.Name | quote }}
{{- end -}}
{{- end -}}

{{- define "ai-platform.redisUrl" -}}
{{- if .Values.secrets.redisUrl -}}
{{ .Values.secrets.redisUrl | quote }}
{{- else -}}
{{ printf "redis://%s-redis-master:6379" .Release.Name | quote }}
{{- end -}}
{{- end -}}
