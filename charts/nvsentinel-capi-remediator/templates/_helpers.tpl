{{/*
Name of the chart, used as the application label.
*/}}
{{- define "nvsentinel-capi-remediator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Name the objects of a release are created under. A release named after the
chart is not repeated in it.
*/}}
{{- define "nvsentinel-capi-remediator.fullname" -}}
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
Labels the Deployment selects its pods by. They must not change between
releases, so nothing versioned goes in here.
*/}}
{{- define "nvsentinel-capi-remediator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nvsentinel-capi-remediator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Labels every object carries.
*/}}
{{- define "nvsentinel-capi-remediator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "nvsentinel-capi-remediator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Name of the ServiceAccount the manager runs as.
*/}}
{{- define "nvsentinel-capi-remediator.serviceAccountName" -}}
{{- default (include "nvsentinel-capi-remediator.fullname" .) .Values.serviceAccount.name }}
{{- end }}
