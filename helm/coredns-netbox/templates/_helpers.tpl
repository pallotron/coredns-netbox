{{/*
Expand the name of the chart.
*/}}
{{- define "coredns-netbox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "coredns-netbox.fullname" -}}
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
Common labels
*/}}
{{- define "coredns-netbox.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{ include "coredns-netbox.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "coredns-netbox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "coredns-netbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Default soft pod anti-affinity — keeps replicas on separate nodes when
affinity is not explicitly overridden.
*/}}
{{- define "coredns-netbox.affinity" -}}
{{- if .Values.affinity }}
{{- toYaml .Values.affinity }}
{{- else }}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        labelSelector:
          matchLabels:
            {{- include "coredns-netbox.selectorLabels" . | nindent 12 }}
        topologyKey: kubernetes.io/hostname
{{- end }}
{{- end }}

{{/*
Name of the ServiceAccount to use.
*/}}
{{- define "coredns-netbox.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "coredns-netbox.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the secret containing the Netbox API token.
*/}}
{{- define "coredns-netbox.tokenSecretName" -}}
{{- if .Values.netbox.existingSecret }}
{{- .Values.netbox.existingSecret }}
{{- else }}
{{- include "coredns-netbox.fullname" . }}
{{- end }}
{{- end }}

{{/*
Comma-separated CoreDNS pod addresses for the sidecar's COREDNS_RELOAD_ADDRS.
When sidecar is a sidecar container (same pod), localhost suffices.
When sidecar.standalone is true, enumerate StatefulSet pod DNS names.
*/}}
{{/*
HTTP base URL of the standalone sidecar service.
Used by zone-init (--fetch-from) and netboxreload source_url.
*/}}
{{- define "coredns-netbox.sidecarHTTPURL" -}}
http://{{ include "coredns-netbox.fullname" . }}-sidecar:8082
{{- end -}}

{{- define "coredns-netbox.reloadAddrs" -}}
{{- $port := .Values.coredns.reloadGRPCPort | default ":8054" | trimPrefix ":" -}}
{{- if .Values.sidecar.standalone -}}
{{- $addrs := list -}}
{{- $name := include "coredns-netbox.fullname" . -}}
{{- $ns := .Release.Namespace -}}
{{- range $i := until (.Values.replicaCount | int) -}}
{{- $addrs = append $addrs (printf "%s-%d.%s-headless.%s.svc.cluster.local:%s" $name $i $name $ns $port) -}}
{{- end -}}
{{- join "," $addrs -}}
{{- else -}}
{{- printf "localhost:%s" $port -}}
{{- end -}}
{{- end -}}
