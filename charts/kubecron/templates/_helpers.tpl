{{/*
Expand the name of the chart.
*/}}
{{- define "kubecron.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kubecron.fullname" -}}
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
Chart label.
*/}}
{{- define "kubecron.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "kubecron.labels" -}}
helm.sh/chart: {{ include "kubecron.chart" . }}
{{ include "kubecron.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "kubecron.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubecron.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "kubecron.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kubecron.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference.
*/}}
{{- define "kubecron.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Name of the kubeconfigs Secret to mount.
*/}}
{{- define "kubecron.kubeconfigsSecretName" -}}
{{- if .Values.kubeconfigs.existingSecret }}
{{- .Values.kubeconfigs.existingSecret }}
{{- else }}
{{- printf "%s-kubeconfigs" (include "kubecron.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Name of the OIDC Secret.
*/}}
{{- define "kubecron.oidcSecretName" -}}
{{- if .Values.oidc.existingSecret }}
{{- .Values.oidc.existingSecret }}
{{- else }}
{{- printf "%s-oidc" (include "kubecron.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The release's mode, validated. "standalone" and "collector" are accepted as the
aliases the binary accepts, and normalised here so every template can compare
against exactly two values.

A typo fails the install rather than silently selecting the default: rendering a
"sever"-mode release as a full UI with suspend/resume/trigger exposed is the
kind of default that is discovered in production.
*/}}
{{- define "kubecron.mode" -}}
{{- $m := .Values.mode | default "ui" -}}
{{- if or (eq $m "ui") (eq $m "standalone") -}}
ui
{{- else if or (eq $m "server") (eq $m "collector") -}}
server
{{- else -}}
{{- fail (printf "\n\nmode=%s is not a KubeCron mode.\n\nChoose one:\n  * mode: ui      — the standalone dashboard (default)\n  * mode: server  — the headless read-only collector API\n" $m) -}}
{{- end -}}
{{- end }}

{{/*
Name of the Secret holding the collector API token, or "" when no token is
configured. Callers use emptiness as the test for "is a front door configured".
*/}}
{{- define "kubecron.apiTokenSecretName" -}}
{{- if .Values.api.existingSecret }}
{{- .Values.api.existingSecret }}
{{- else if .Values.api.token }}
{{- printf "%s-api" (include "kubecron.fullname" .) }}
{{- end }}
{{- end }}

{{/*
The Secret holding alert destinations: an existing one, or the chart's own
when any destination is given inline. Empty when neither.
*/}}
{{- define "kubecron.alertsSecretName" -}}
{{- if .Values.alerts.existingSecret }}
{{- .Values.alerts.existingSecret }}
{{- else if or .Values.alerts.webhookURL .Values.alerts.slackWebhookURL .Values.alerts.teamsWebhookURL .Values.alerts.smtp.password }}
{{- printf "%s-alerts" (include "kubecron.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Whether /api/v1 can actually be reached by a program, which is the only honest
condition for advertising this Service to a console.

Not the same question as the mode. A `server` release always qualifies: it has
no browser flow at all. A `ui` release qualifies only when something a program
can present will open the door — an API token, or no OIDC at all. A `ui` release
with OIDC and no token answers every API request with a redirect to an identity
provider, so labelling it discoverable would advertise a door that cannot be
opened, and a console would report a collector it can never read.
*/}}
{{- define "kubecron.programReadable" -}}
{{- if eq (include "kubecron.mode" .) "server" -}}
true
{{- else if ne (include "kubecron.apiTokenSecretName" .) "" -}}
true
{{- else if not .Values.oidc.enabled -}}
true
{{- end -}}
{{- end }}

{{/*
Name of the PVC.
*/}}
{{- define "kubecron.pvcName" -}}
{{- if .Values.persistence.existingClaim }}
{{- .Values.persistence.existingClaim }}
{{- else }}
{{- printf "%s-data" (include "kubecron.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Refuse to render a deployment that is reachable from outside the cluster while
no front door is configured for the mode it runs in (AUDIT SEC-28).

Both exposure paths are covered, not just the Ingress: a LoadBalancer or
NodePort Service reaches outside the cluster just as effectively. Any new
exposure path added to this chart must be added here.

What "no front door" means differs by mode, and so does what it costs:

  * mode: ui with oidc.enabled=false — the operator gate in
    internal/api/server.go becomes a deliberate pass-through and the auth
    middleware is never installed. Every route is anonymous, including POST
    suspend/resume/trigger, which act on *every* cluster whose kubeconfig is
    mounted. A single unauthenticated request can suspend a backup CronJob
    fleet-wide.

  * mode: server with no api.token — there are no mutating routes to reach, so
    this is disclosure rather than control: the full CronJob inventory,
    schedules, run outcomes, resource usage and captured log bodies, plus
    /metrics. Milder, and still not something to arrive at by leaving a default
    alone.

The escape hatch exists because "reachable only from a trusted mesh or VPN,
auth handled upstream" is a legitimate deployment — but it has to be stated.
*/}}
{{- define "kubecron.validateExposure" -}}
{{- $mode := include "kubecron.mode" . -}}
{{- $guarded := ternary .Values.oidc.enabled (ne (include "kubecron.apiTokenSecretName" .) "") (eq $mode "ui") -}}
{{- if not $guarded }}
{{- if not .Values.security.acknowledgeInsecureExposure }}
{{- $fix := ternary "set oidc.enabled=true (recommended), or" "set api.token (or api.existingSecret) to require a bearer token (recommended), or" (eq $mode "ui") -}}
{{- $risk := ternary "Every endpoint becomes anonymous, including suspend/resume/trigger, which act on every cluster whose kubeconfig is mounted." "Every /api/v1 route and /metrics answers anonymously, disclosing the whole CronJob inventory, run outcomes and captured log bodies." (eq $mode "ui") -}}
{{- if .Values.ingress.enabled }}
{{- fail (printf "\n\nSEC-28: ingress.enabled=true in mode=%s with no authentication configured.\n%s\n\nChoose one:\n  * %s\n  * set security.acknowledgeInsecureExposure=true if the Ingress is on a trusted network and authentication is enforced upstream.\n" $mode $risk $fix) }}
{{- end }}
{{- if ne .Values.service.type "ClusterIP" }}
{{- fail (printf "\n\nSEC-28: service.type=%s in mode=%s reaches outside the cluster with no authentication configured.\n%s\n\nChoose one:\n  * %s\n  * keep service.type=ClusterIP and use `kubectl port-forward`, or\n  * set security.acknowledgeInsecureExposure=true if the network is trusted and authentication is enforced upstream.\n" .Values.service.type $mode $risk $fix) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
