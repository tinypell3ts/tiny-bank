{{/* Service name; fails the render early if a values file forgot it. */}}
{{- define "service.name" -}}
{{- required "values: name is required" .Values.name -}}
{{- end -}}

{{/* Labels used to select this service's pods. Must never change after first install. */}}
{{- define "service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Full label set for every resource. */}}
{{- define "service.labels" -}}
{{ include "service.selectorLabels" . }}
app.kubernetes.io/part-of: tiny-bank
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}
