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
{{/*
Public base URL of the controller: controller.externalUrl when set, else
derived from the first ingress host (https when an ingress.tls entry lists that
host, http otherwise), else empty.
*/}}
{{- define "unified-cd.externalUrl" -}}
{{- if .Values.controller.externalUrl -}}
{{- .Values.controller.externalUrl -}}
{{- else if and .Values.ingress.enabled .Values.ingress.hosts (index .Values.ingress.hosts 0).host -}}
{{- $host := (index .Values.ingress.hosts 0).host -}}
{{- $tls := false -}}
{{- range .Values.ingress.tls -}}
{{- if has $host (.hosts | default list) -}}{{- $tls = true -}}{{- end -}}
{{- end -}}
{{- if $tls -}}https://{{ $host }}{{- else -}}http://{{ $host }}{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Name of the cluster the chart-computed policies refer to: the first entry of
the (user-supplied or default) kubernetesClusters list that has no kubeconfig,
that is the cluster the controller runs in. "in-cluster" when the list has no
such entry; the render-time check in unified-cd.validate then names the problem.
*/}}
{{- define "unified-cd.ownClusterName" -}}
{{- $own := "" -}}
{{- range (dig "agentAuth" "kubernetesClusters" (list (dict "name" "in-cluster")) .Values.controller.config) -}}
{{- if and (not $own) (kindIs "map" .) (not .kubeconfig) .name -}}{{- $own = .name -}}{{- end -}}
{{- end -}}
{{- default "in-cluster" $own -}}
{{- end -}}

{{/*
The Kubernetes agent config as rendered into its ConfigMap: agent.config
deep-merged over the computed default. Shared by the ConfigMap and the values
derived from it (the store-credential ServiceAccounts, the namespace check).
*/}}
{{- define "unified-cd.agentConfig" -}}
{{- include "unified-cd.mergeConfig" (dict "base" (include "unified-cd.agentConfigDefault" . | fromYaml) "over" .Values.agent.config) -}}
{{- end -}}

{{/*
ServiceAccounts the default store-credential policy allows, as a YAML list:
"default" plus every ServiceAccount the rendered agent config can run a job Pod
under (podTemplates.<name>.spec.serviceAccountName or .serviceAccount, and
jobPodPolicy.allowServiceAccounts), sorted and de-duplicated.
*/}}
{{- define "unified-cd.storeServiceAccounts" -}}
{{- $agent := include "unified-cd.agentConfig" . | fromYaml -}}
{{- $sas := list "default" -}}
{{- range $name, $tpl := (get $agent "podTemplates" | default dict) -}}
{{- if kindIs "map" $tpl -}}
{{- $spec := get $tpl "spec" | default dict -}}
{{- if kindIs "map" $spec -}}
{{- range (list "serviceAccountName" "serviceAccount") -}}
{{- $sa := get $spec . -}}
{{- if and $sa (kindIs "string" $sa) -}}{{- $sas = append $sas $sa -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $policy := get $agent "jobPodPolicy" | default dict -}}
{{- if kindIs "map" $policy -}}
{{- range (get $policy "allowServiceAccounts" | default list) -}}
{{- $sas = append $sas . -}}
{{- end -}}
{{- end -}}
{{- toYaml ($sas | uniq | sortAlpha) -}}
{{- end -}}

{{- define "unified-cd.controllerConfigDefault" -}}
{{- with (include "unified-cd.externalUrl" .) }}
externalUrl: {{ . | quote }}
{{- end }}
{{- with .Values.controller.trustedProxies }}
trustedProxies:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.controller.serviceAccounts }}
serviceAccounts:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.controller.tokens }}
tokens:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with .Values.controller.ui }}
ui:
  {{- toYaml . | nindent 2 }}
{{- end }}
agentAuth:
  kubernetesClusters:
    - name: in-cluster
  kubernetesEnrollmentPolicies:
    - name: unified-cd-k8s-agents
      cluster: {{ include "unified-cd.ownClusterName" . | quote }}
      namespaces: [{{ include "unified-cd.namespace" . | quote }}]
      serviceAccounts: [unified-cd-k8s-agent]
      allowedLabels: ["kind:kubernetes"]
      requiredLabels: ["kind:kubernetes"]
      accessTokenTTL: 1h
      enabled: true
  {{- /*
  Lets the agent's job Pods ask the controller for presigned object URLs for
  the artifact sidecar (the field keeps its pre-presign name). The allowed
  ServiceAccounts are "default" plus those the rendered agent config can run
  job Pods under (podTemplates, jobPodPolicy.allowServiceAccounts). A
  kubernetesStoreCredentialPolicies list in controller.config replaces this
  one entirely.
  */}}
  kubernetesStoreCredentialPolicies:
    - cluster: {{ include "unified-cd.ownClusterName" . | quote }}
      namespaces: [{{ .Values.jobNamespace.name | quote }}]
      serviceAccounts:
        {{- include "unified-cd.storeServiceAccounts" . | nindent 8 }}
{{- end -}}

{{/*
The controller config as rendered into its ConfigMap: controller.config
deep-merged over the computed default. Shared by the ConfigMap and the RBAC
that is derived from the config.
*/}}
{{- define "unified-cd.controllerConfig" -}}
{{- include "unified-cd.mergeConfig" (dict "base" (include "unified-cd.controllerConfigDefault" . | fromYaml) "over" .Values.controller.config) -}}
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
{{- if and .Values.ingress.enabled (not .Values.controller.enabled) -}}
{{- fail "ingress.enabled requires controller.enabled: the Ingress routes to the controller Service" -}}
{{- end -}}
{{- if and .Values.ingress.enabled (include "unified-cd.trialSecret" .) -}}
{{- fail "ingress.enabled with the bundled trial credentials would expose the trial database password; use the production values" -}}
{{- end -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.hosts) -}}
{{- fail "ingress.enabled requires at least one entry in ingress.hosts" -}}
{{- end -}}
{{- if .Values.agent.enabled -}}
{{- $agentNamespace := get (include "unified-cd.agentConfig" . | fromYaml) "namespace" -}}
{{- if ne $agentNamespace .Values.jobNamespace.name -}}
{{- fail (printf "agent.config.namespace (%s) differs from jobNamespace.name (%s): the job Role, the Namespace and the controller policies follow jobNamespace.name, so set jobNamespace.name instead of agent.config.namespace" $agentNamespace .Values.jobNamespace.name) -}}
{{- end -}}
{{- end -}}
{{- if .Values.controller.enabled -}}
{{- /*
The chart's own enrollment and store-credential policies name the first
cluster without a kubeconfig. When the user replaced kubernetesClusters with a
list that has none, those policies would name a cluster that does not exist
and the controller would exit at startup, so fail here unless the user also
supplied both policy lists (then the policies are theirs to get right).
*/ -}}
{{- $userAuth := dig "agentAuth" dict .Values.controller.config -}}
{{- $names := list -}}
{{- range (dig "agentAuth" "kubernetesClusters" (list (dict "name" "in-cluster")) .Values.controller.config) -}}{{- $names = append $names .name -}}{{- end -}}
{{- $usesComputed := not (and (hasKey $userAuth "kubernetesEnrollmentPolicies") (hasKey $userAuth "kubernetesStoreCredentialPolicies")) -}}
{{- if and $usesComputed (not (has (include "unified-cd.ownClusterName" .) $names)) -}}
{{- fail (printf "controller.config.agentAuth.kubernetesClusters has no cluster without a kubeconfig (clusters: %s), so the chart's default kubernetesEnrollmentPolicies and kubernetesStoreCredentialPolicies have no cluster to name. Add a cluster entry without a kubeconfig, or supply both kubernetesEnrollmentPolicies and kubernetesStoreCredentialPolicies in controller.config" (join ", " $names)) -}}
{{- end -}}
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

{{/*
Renders "true" when the initial admin token Job runs and renders nothing
otherwise: only with the controller enabled and controller.initialAdminToken.generate
true (the default, for trial and production installs alike). Use as:
  {{ if include "unified-cd.initialTokenGenerate" . }}
*/}}
{{- define "unified-cd.initialTokenGenerate" -}}
{{- if and .Values.controller.enabled .Values.controller.initialAdminToken.generate -}}
true
{{- end -}}
{{- end -}}

{{/*
Shell lines (for NOTES.txt) that read the generated initial admin token and
exchange it for an admin PAT. Never renders the token itself.
*/}}
{{- define "unified-cd.bootstrapSteps" -}}
  export UNIFIED_SERVER=http://localhost:8080
  export UNIFIED_INITIAL_ADMIN_TOKEN="$(kubectl -n {{ .Release.Namespace }} get secret {{ .Values.controller.initialAdminToken.secretName }} -o jsonpath='{.data.UNIFIED_INITIAL_ADMIN_TOKEN}' | base64 -d)"
  unified-cli bootstrap

The last command prints the PAT; use it as UNIFIED_TOKEN for unified-cli and
paste it into the web UI sign-in form.
{{- end -}}
