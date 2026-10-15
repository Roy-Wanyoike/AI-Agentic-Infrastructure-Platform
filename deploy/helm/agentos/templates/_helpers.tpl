{{/* Shared helpers for the agentos chart (issue #117). */}}

{{/* Chart name (nameOverride-aware). */}}
{{- define "agentos.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified object basename: fullnameOverride or <release>-<chart>. */}}
{{- define "agentos.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "agentos.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/* Per-component basename: include "agentos.componentName" (list "root" $ "component" "api") */}}
{{- define "agentos.componentName" -}}
{{- printf "%s-%s" (include "agentos.fullname" (index . 0)) (index . 1) -}}
{{- end -}}

{{/* Chart label (appVersion). */}}
{{- define "agentos.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "agentos.labels" -}}
app.kubernetes.io/part-of: agentos
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ include "agentos.chart" . }}
{{ include "agentos.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/* Selector labels (stable across upgrades). */}}
{{- define "agentos.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agentos.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Component selector labels: include "agentos.componentSelectorLabels" (list $ "api")

NOTE (issue #117 fixup): app.kubernetes.io/name MUST be the per-component name
(fullname-component), NOT the chart-level name. Every selector in this chart --
Deployment/StatefulSet spec.selector, Service/PDB selectors, NetworkPolicy
podSelectors and the data-tier default-deny -- matches on this value, and
spec.selector is REQUIRED to match the pod template labels (the API server
rejects the object otherwise). Keep them in lockstep.
*/}}
{{- define "agentos.componentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "agentos.componentName" . }}
app.kubernetes.io/instance: {{ (index . 0).Release.Name }}
app.kubernetes.io/component: {{ index . 1 }}
{{- end -}}

{{/* ServiceAccount name: include "agentos.serviceAccountName" (list $ "api") */}}
{{- define "agentos.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{ include "agentos.componentName" . }}
{{- else -}}
{{- printf "default" -}}
{{- end -}}
{{- end -}}

{{/* Secret name (existingSecret wins over the rendered one). */}}
{{- define "agentos.secretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- include "agentos.componentName" (list . "secrets") -}}
{{- end -}}
{{- end -}}

{{/* ConfigMap name. */}}
{{- define "agentos.configName" -}}
{{- include "agentos.componentName" (list . "config") -}}
{{- end -}}

{{/* OTEL sidecar collector ConfigMap name. */}}
{{- define "agentos.otelConfigName" -}}
{{- include "agentos.componentName" (list . "otel-config") -}}
{{- end -}}

{{/* Full image reference honoring global.imageRegistry:
     include "agentos.image" (list $ .Values.api.image.repository)
     When global.imageRegistry is set, the registry replaces the repository's
     registry host (last path segment is kept, e.g. agentos-api). */}}
{{- define "agentos.image" -}}
{{- $repository := index . 1 -}}
{{- if index (index . 0).Values.global "imageRegistry" -}}
{{- printf "%s/%s" (index (index . 0).Values.global "imageRegistry") (regexReplaceAll "^.*/" $repository "") -}}
{{- else -}}
{{- $repository -}}
{{- end -}}
{{- end -}}

{{/* Pod-level securityContext used by the distroless app workloads. */}}
{{- define "agentos.podSecurityContext" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{/* Container-level hardening (distroless app containers). */}}
{{- define "agentos.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop:
    - ALL
{{- end -}}

{{/* OpenTelemetry env injected on the app container when the sidecar is on. */}}
{{- define "agentos.otelEnv" -}}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: http://localhost:4318
- name: OTEL_SERVICE_NAME
  value: {{ index . 1 | quote }}
{{- end -}}
