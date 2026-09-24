{{/*
Fully-qualified image reference for a chart-built image (genset, switchboard,
propulsion, battery, auxload, telemetry-writer, playground): prefixes
global.imageRegistry, falls back to global.imageTag when a service doesn't
pin its own tag. Called as:
{{ include "diagent.customImage" (dict "global" .Values.global "image" .Values.genset.image) }}
*/}}
{{- define "diagent.customImage" -}}
{{- $tag := .image.tag | default .global.imageTag -}}
{{- if .global.imageRegistry -}}
{{ .global.imageRegistry }}/{{ .image.repository }}:{{ $tag }}
{{- else -}}
{{ .image.repository }}:{{ $tag }}
{{- end -}}
{{- end -}}

{{/*
Image reference for a publicly-pulled image (kafka, influxdb, grafana): used
as-is, never prefixed with global.imageRegistry.
*/}}
{{- define "diagent.publicImage" -}}
{{ .repository }}:{{ .tag }}
{{- end -}}

{{/*
Merge a per-service nodeSelector (if any) with global.nodeSelector. Called
as: {{- include "diagent.nodeSelector" (dict "local" .Values.genset.nodeSelector "global" $.Values.global.nodeSelector) }}
*/}}
{{- define "diagent.nodeSelector" -}}
{{- $merged := merge (.local | default dict) (.global | default dict) -}}
{{- if $merged }}
nodeSelector:
{{ toYaml $merged | indent 2 }}
{{- end -}}
{{- end -}}

{{/*
Pod template annotation that changes on every "helm upgrade" invocation, so
Deployments always roll a new pod even when the rendered spec is otherwise
unchanged (e.g. rebuilding an image under a floating ":latest" tag doesn't
change the manifest text, so Kubernetes wouldn't otherwise notice). Set
global.forceRollout=false to disable. Include as a sibling of the template's
"labels:" key, e.g.:
      labels:
        app: genset
      {{- include "diagent.podAnnotations" . | nindent 6 }}
*/}}
{{- define "diagent.podAnnotations" -}}
{{- if ne .Values.global.forceRollout false }}
annotations:
  diagent.io/restartedAt: {{ now | date "2006-01-02T15:04:05Z07:00" | quote }}
{{- end -}}
{{- end -}}
