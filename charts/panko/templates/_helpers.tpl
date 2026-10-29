{{/* Common labels; app is kept as the selector label the existing tooling uses. */}}
{{- define "panko.labels" -}}
app.kubernetes.io/part-of: panko
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{/* Keeps an object on helm uninstall and GitOps pruning. */}}
{{- define "panko.retain" -}}
{{- if .Values.retain }}
helm.sh/resource-policy: keep
argocd.argoproj.io/sync-options: Prune=false,Delete=false
kustomize.toolkit.fluxcd.io/prune: disabled
{{- end }}
{{- end }}

{{- define "panko.serverImage" -}}
{{ .Values.images.server.repository }}:{{ .Values.images.server.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "panko.moduleMirrorImage" -}}
{{ .Values.images.moduleMirror.repository }}:{{ .Values.images.moduleMirror.tag | default .Chart.AppVersion }}
{{- end }}

{{- define "panko.pullSecrets" -}}
{{- with .Values.images.pullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end }}

{{/* Restricted container security context shared by every Panko container. */}}
{{- define "panko.restricted" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities: {drop: [ALL]}
{{- end }}

{{- define "panko.podSecurity" -}}
runAsNonRoot: true
runAsUser: 1000
runAsGroup: 1000
fsGroup: 1000
seccompProfile: {type: RuntimeDefault}
{{- end }}

{{/* The installation configuration as panko.json; the setup Job validates it. */}}
{{- define "panko.installation" -}}
{{- $i := .Values.installation -}}
{{- $c := dict "teams" $i.teams "s3" (default false $i.s3) -}}
{{- if $i.egress }}{{ $_ := set $c "egress" true }}{{ end -}}
{{- with $i.egressTargets }}{{ $_ := set $c "egressTargets" . }}{{ end -}}
{{- with $i.runnerPools }}{{ $_ := set $c "runnerPools" . }}{{ end -}}
{{- with $i.runnerEndpoint }}{{ with .names }}{{ $_ := set $c "runnerEndpoint" (dict "names" .) }}{{ end }}{{ end -}}
{{- $c | toPrettyJson -}}
{{- end }}

{{/* The hook's runner pool view: pool definitions and which team binds to which pool. */}}
{{- define "panko.runnerPools" -}}
{{- $pools := dict -}}
{{- $teams := dict -}}
{{- range .Values.installation.runnerPools -}}
{{- $pool := dict -}}
{{- with .module }}{{ $_ := set $pool "module" . }}{{ end -}}
{{- with .leaseSeconds }}{{ $_ := set $pool "leaseSeconds" . }}{{ end -}}
{{- with .heartbeatSeconds }}{{ $_ := set $pool "heartbeatSeconds" . }}{{ end -}}
{{- $_ := set $pools .name $pool -}}
{{- $name := .name -}}
{{- range .teams }}{{ $_ := set $teams . $name }}{{ end -}}
{{- end -}}
{{- dict "pools" $pools "teams" $teams | toJson -}}
{{- end }}
