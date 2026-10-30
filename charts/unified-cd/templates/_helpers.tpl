{{/*
Namespace every namespaced object is created in: the release namespace.
*/}}
{{- define "unified-cd.namespace" -}}
{{- .Release.Namespace -}}
{{- end -}}

{{/*
Controller image reference; the tag defaults to the chart appVersion.
*/}}
{{- define "unified-cd.controllerImage" -}}
{{- printf "%s:%s" .Values.controller.image.repository (default .Chart.AppVersion .Values.controller.image.tag) -}}
{{- end -}}

{{/*
Kubernetes agent image reference; the tag defaults to the chart appVersion.
*/}}
{{- define "unified-cd.agentImage" -}}
{{- printf "%s:%s" .Values.agent.image.repository (default .Chart.AppVersion .Values.agent.image.tag) -}}
{{- end -}}

{{/*
Standard labels applied to every object.
*/}}
{{- define "unified-cd.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{/*
Renders "true" when the chart renders the bundled trial Secret (trial
credentials) and renders nothing otherwise. Use as:
  {{ if include "unified-cd.trialSecret" . }}
*/}}
{{- define "unified-cd.trialSecret" -}}
{{- if and .Values.controller.enabled .Values.bundledStores.enabled (not .Values.controller.existingSecret) -}}
true
{{- end -}}
{{- end -}}

{{/*
Name of the Secret the controller reads its environment from: the user's
controller.existingSecret, or the default "unified-cd-controller" (the trial
Secret when the chart renders it).
*/}}
{{- define "unified-cd.controllerSecretName" -}}
{{- default "unified-cd-controller" .Values.controller.existingSecret -}}
{{- end -}}

{{/*
Chart-computed controller config, rendered as YAML. controller.config is
deep-merged over it by the ConfigMap template.
*/}}
{{- define "unified-cd.controllerConfigDefault" -}}
agentAuth:
  kubernetesClusters:
    - name: in-cluster
  kubernetesEnrollmentPolicies:
    - name: unified-cd-k8s-agents
      cluster: in-cluster
      namespaces: [{{ include "unified-cd.namespace" . | quote }}]
      serviceAccounts: [unified-cd-k8s-agent]
      allowedLabels: ["kind:kubernetes"]
      requiredLabels: ["kind:kubernetes"]
      accessTokenTTL: 1h
      enabled: true
{{- if .Values.bundledStores.enabled }}
  {{- /*
  Lets the agent's job Pods obtain object-store credentials for the artifact
  sidecar (the agent runs with sidecarS3SecretMode: broker when the bundled
  stores are enabled). Job Pods run under the job namespace's default
  ServiceAccount; name a dedicated one through controller.config when you use
  a custom ServiceAccount.
  */}}
  kubernetesStoreCredentialPolicies:
    - cluster: in-cluster
      namespaces: [{{ .Values.jobNamespace.name | quote }}]
      serviceAccounts: [default]
{{- end }}
{{- end -}}

{{/*
Render-time value checks. Included from every top-level template so an invalid
combination fails the render with a message naming the value to change.
*/}}
{{- define "unified-cd.validate" -}}
{{- if and (not .Values.controller.enabled) (not .Values.agent.enabled) -}}
{{- fail "nothing to install: enable controller.enabled or agent.enabled" -}}
{{- end -}}
{{- if and .Values.bundledStores.enabled (not .Values.controller.enabled) -}}
{{- fail "bundledStores.enabled requires controller.enabled" -}}
{{- end -}}
{{- if and (not .Values.agent.server) (not .Values.controller.enabled) -}}
{{- fail "agent.server is required when controller.enabled is false" -}}
{{- end -}}
{{- if and .Values.agent.server (not (hasPrefix "https://" .Values.agent.server)) -}}
{{- fail "agent.server must be an https:// URL" -}}
{{- end -}}
{{- if and .Values.ingress.enabled (include "unified-cd.trialSecret" .) -}}
{{- fail "ingress.enabled with the bundled trial credentials would expose dev-token-change-me: set controller.existingSecret to a Secret you created, or disable ingress" -}}
{{- end -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.hosts) -}}
{{- fail "ingress.enabled requires at least one entry in ingress.hosts" -}}
{{- end -}}
{{- end -}}

{{/*
Recursive config merge: include with (dict "base" <map> "over" <map>). Maps
merge key by key, a list or scalar in "over" replaces the one in "base", and
the user's value always wins, including false, 0 and "" (Sprig mergeOverwrite
skips those zero values when the key already exists). Returns YAML; decode it
with fromYaml.
*/}}
{{- define "unified-cd.mergeConfig" -}}
{{- $out := deepCopy .base -}}
{{- range $k, $v := .over -}}
{{- if and (kindIs "map" $v) (hasKey $out $k) (kindIs "map" (get $out $k)) -}}
{{- $_ := set $out $k (include "unified-cd.mergeConfig" (dict "base" (get $out $k) "over" $v) | fromYaml) -}}
{{- else -}}
{{- $_ := set $out $k $v -}}
{{- end -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
Chart-computed Kubernetes agent config, rendered as YAML. agent.config is
merged over it by the agent ConfigMap template. With agent.server empty the
agent talks to the in-cluster controller Service over plain HTTP (the
allowInsecureHTTP opt-in); an explicit agent.server is always https:// and
carries no allowInsecureHTTP.
*/}}
{{- define "unified-cd.agentConfigDefault" -}}
{{- if .Values.agent.server }}
server: {{ .Values.agent.server | quote }}
{{- else }}
server: {{ printf "http://unified-cd-controller.%s.svc.cluster.local" (include "unified-cd.namespace" .) | quote }}
allowInsecureHTTP: true
{{- end }}
enrollmentPolicy: unified-cd-k8s-agents
serviceAccountTokenFile: /var/run/secrets/unified-cd-agent/token
labels:
  - kind:kubernetes
namespace: {{ .Values.jobNamespace.name | quote }}
maxConcurrent: 5
podImage: golang:1.24-alpine
{{- if .Values.bundledStores.enabled }}
sidecarS3SecretMode: broker
{{- end }}
{{- end -}}

{{/*
Renders "true" when the KEK Job runs and renders nothing otherwise: only with
the controller enabled, and controller.kek.generate when set, else
bundledStores.enabled. Use as:
  {{ if include "unified-cd.kekGenerate" . }}
*/}}
{{- define "unified-cd.kekGenerate" -}}
{{- if .Values.controller.enabled -}}
{{- $gen := .Values.controller.kek.generate -}}
{{- if kindIs "invalid" $gen -}}
{{- $gen = .Values.bundledStores.enabled -}}
{{- end -}}
{{- if $gen -}}
true
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Garage server configuration (garage.toml). A helper so the Deployment can
checksum it without including its own template file.
*/}}
{{- define "unified-cd.garageToml" -}}
# Keep in sync with deployments/garage/garage.toml.
metadata_dir = "/data/meta"
data_dir = "/data/data"
db_engine = "sqlite"
replication_factor = 1

rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"

[admin]
api_bind_addr = "[::]:3903"
{{- end -}}
