{{/*
The server container's environment. Every name here must be a row of
docs/configuration.md (internal/modulegate/chart_env_test.go); each block
names the value that switches it on.
*/}}
{{- define "muster.serverEnv" -}}
{{- $gen := include "muster.generatedSecretName" . -}}
{{- $full := include "muster.fullname" . -}}
{{- $v := .Values -}}
- name: MUSTER_PORT
  value: {{ $v.service.port | quote }}
{{- if $v.postgres.enabled }}
{{ include "muster.secretEnv" (dict "name" "DATABASE_URL" "existing" $v.postgres.existingSecret "ctx" .) }}
{{- else }}
{{ include "muster.secretEnv" (dict "name" "DATABASE_URL" "existing" (required "externalDatabase.existingSecret is required when postgres.enabled is false" $v.externalDatabase.existingSecret) "key" $v.externalDatabase.key "ctx" .) }}
{{- end }}
{{- include "muster.env" (dict "name" "MUSTER_PUBLIC_URL" "value" $v.publicUrl) }}
{{- if $v.standalone }}
- name: MUSTER_STANDALONE
  value: "1"
{{- end }}
{{- if $v.mirror }}
- name: MUSTER_MIRROR
  value: "1"
{{- end }}
- name: MUSTER_SECURE_COOKIES
  value: {{ ternary "1" "0" $v.secureCookies | quote }}
{{- if or $v.clientAddress.trustedProxies $v.clientAddress.header }}
{{- /* Both or neither: the server refuses to boot on one alone, so fail the
render instead of shipping a pod that crash-loops. */ -}}
{{- if not (and $v.clientAddress.trustedProxies $v.clientAddress.header) }}
{{- fail "clientAddress: set both trustedProxies and header, or neither" }}
{{- end }}
- name: MUSTER_TRUSTED_PROXIES
  value: {{ join "," $v.clientAddress.trustedProxies | quote }}
- name: MUSTER_CLIENT_IP_HEADER
  value: {{ $v.clientAddress.header | quote }}
{{- end }}
- name: MUSTER_AUTH_MODE
  value: {{ $v.auth.mode | quote }}
{{- if eq $v.auth.mode "password" }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_UI_PASSWORD" "existing" $v.auth.existingSecret "ctx" .) }}
{{- end }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_HOOK_TOKEN" "existing" $v.auth.existingSecret "ctx" .) }}
{{- if include "muster.gotrueOn" . }}
{{- if not $v.gotrue.enabled }}
{{- /* Your own GoTrue: without these the server would be pointed at a bundled
GoTrue that is not running, with a JWT secret no Secret holds. */ -}}
{{- if not $v.gotrue.url }}
{{- fail "auth.mode=gotrue with gotrue.enabled=false: set gotrue.url (your GoTrue) and gotrue.existingSecret (its JWT secret, key MUSTER_GOTRUE_JWT_SECRET), or set gotrue.enabled=true for the bundled one" }}
{{- end }}
{{- if not $v.gotrue.existingSecret }}
{{- fail "auth.mode=gotrue with gotrue.enabled=false: set gotrue.existingSecret (your GoTrue's JWT secret, key MUSTER_GOTRUE_JWT_SECRET); the chart generates it only for the bundled GoTrue" }}
{{- end }}
{{- end }}
- name: MUSTER_GOTRUE_URL
  value: {{ default (printf "http://%s-gotrue:9999" $full) $v.gotrue.url | quote }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_GOTRUE_JWT_SECRET" "existing" $v.gotrue.existingSecret "ctx" .) }}
- name: MUSTER_BROWSER_PORT
  value: {{ $v.service.browserPort | quote }}
{{- end }}
{{- if $v.plugins }}
- name: MUSTER_PLUGINS
  value: "1"
{{- end }}
{{- if $v.chief.taskRouting }}
- name: MUSTER_TASK_ROUTING
  value: "1"
{{- end }}
{{- if $v.chief.dispatchOwners }}
- name: MUSTER_CHIEF_DISPATCH_OWNERS
  value: {{ join "," $v.chief.dispatchOwners | quote }}
{{- end }}
{{- if $v.chief.openrouter.enabled }}
- name: MUSTER_CHIEF_LLM
  value: openrouter
{{- include "muster.env" (dict "name" "MUSTER_CHIEF_LLM_MODEL" "value" $v.chief.openrouter.model) }}
{{- end }}
{{- if $v.chief.openrouterSessions.enabled }}
- name: MUSTER_CHIEF_SESSIONS_OPENROUTER
  value: "1"
{{ include "muster.secretEnv" (dict "name" "MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY" "existing" (required "chief.openrouterSessions.existingSecret is required (key MUSTER_CHIEF_SESSIONS_OPENROUTER_KEY)" $v.chief.openrouterSessions.existingSecret) "ctx" .) }}
{{- include "muster.env" (dict "name" "MUSTER_CHIEF_SESSIONS_DAILY_USD" "value" $v.chief.openrouterSessions.dailyUSD) }}
{{- include "muster.env" (dict "name" "MUSTER_CHIEF_SESSIONS_SESSION_USD" "value" $v.chief.openrouterSessions.sessionUSD) }}
{{- end }}
{{- if include "muster.connectOn" . }}
- name: MUSTER_CONNECT_PORT
  value: {{ $v.service.connectPort | quote }}
- name: MUSTER_AGENT_CONNECT_URL
  value: {{ default (printf "http://%s:%v" (include "muster.svcHost" .) $v.service.connectPort) $v.agents.connectUrl | quote }}
{{- end }}
{{- if $v.connectors.enabled }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_CONNECTOR_ENCRYPTION_KEY" "existing" $v.connectors.existingSecret "ctx" .) }}
{{- end }}
{{- if $v.agents.enabled }}
- name: MUSTER_AGENT_PROVISIONER
  value: {{ $v.agents.provisioner | quote }}
- name: MUSTER_AGENT_IMAGE_REPO
  value: {{ required "agents.gatewayImage.repository is required with agents.enabled (the gateway-kind agent image)" $v.agents.gatewayImage.repository | quote }}
{{- include "muster.env" (dict "name" "MUSTER_AGENT_IMAGE_TAG" "value" $v.agents.gatewayImage.tag) }}
- name: MUSTER_AGENT_API_URL
  value: {{ default (printf "http://%s:%v" (include "muster.svcHost" .) $v.service.port) $v.agents.apiUrl | quote }}
- name: MUSTER_AGENT_NAMESPACE_PREFIX
  value: {{ $v.agents.namespacePrefix | quote }}
{{- $kinds := $v.agents.kinds -}}
{{- if and $v.agents.claudeCode.enabled (not (has "claude-code" $kinds)) -}}
{{- $kinds = append $kinds "claude-code" -}}
{{- end }}
- name: MUSTER_AGENT_KINDS
  value: {{ join "," (uniq $kinds) | quote }}
{{- include "muster.env" (dict "name" "MUSTER_AGENT_MODEL" "value" $v.agents.model) }}
{{- if $v.agents.workspace.persist }}
- name: MUSTER_AGENT_WORKSPACE_PERSIST
  value: "1"
{{- include "muster.env" (dict "name" "MUSTER_AGENT_WORKSPACE_STORAGE_CLASS" "value" $v.agents.workspace.storageClass) }}
{{- end }}
{{- if eq $v.agents.provisioner "kubernetes" }}
- name: MUSTER_AGENT_NETPOL_FROM_NAMESPACE
  value: {{ .Release.Namespace | quote }}
- name: MUSTER_AGENT_NETPOL_FROM_POD_LABELS
  value: {{ include "muster.selectorLabels" . | fromYaml | include "muster.kvList" | quote }}
{{- if eq $v.agents.rbac.layout "least-privilege" }}
- name: MUSTER_AGENT_NAMESPACE_CLUSTERROLE
  value: {{ include "muster.agentNamespaceRole" . | quote }}
{{- end }}
{{- end }}
{{- if $v.agents.gateway.enabled }}
- name: MUSTER_AGENT_GATEWAY
  value: hooks-sha256
- name: MUSTER_AGENT_GATEWAY_MODEL
  value: {{ required "agents.gateway.model is required with agents.gateway.enabled" $v.agents.gateway.model | quote }}
- name: MUSTER_AGENT_RUNTIME_CONFIG
  valueFrom:
    configMapKeyRef:
      name: {{ $full }}-agent-runtime
      key: runtime-config
- name: MUSTER_AGENT_RUNTIME_INSTALL
  valueFrom:
    configMapKeyRef:
      name: {{ $full }}-agent-runtime
      key: runtime-install
{{- end }}
{{- if $v.agents.claudeCode.enabled }}
- name: MUSTER_AGENT_CC_IMAGE
  value: {{ required "agents.claudeCode.image is required with agents.claudeCode.enabled (pinned: a tag or a digest)" $v.agents.claudeCode.image | quote }}
- name: MUSTER_AGENT_CC_ACCOUNTS
  value: {{ join "," (required "agents.claudeCode.accounts names no account" $v.agents.claudeCode.accounts) | quote }}
{{- include "muster.env" (dict "name" "MUSTER_AGENT_CC_STORAGE_SIZE" "value" $v.agents.claudeCode.storageSize) }}
{{- end }}
{{- end }}
{{- if $v.officers.enabled }}
- name: MUSTER_OFFICERS
  value: {{ toJson $v.officers.config | quote }}
{{- if $v.officers.egressCidrs }}
- name: MUSTER_OFFICER_EGRESS_CIDRS
  value: {{ join "," $v.officers.egressCidrs | quote }}
{{- end }}
{{- include "muster.env" (dict "name" "MUSTER_OFFICER_EGRESS_DNS" "value" $v.officers.egressDns) }}
{{- end }}
{{- if $v.githubApp.enabled }}
- name: MUSTER_GITHUB_APP_ID
  value: {{ required "githubApp.appId is required with githubApp.enabled" $v.githubApp.appId | toString | quote }}
- name: MUSTER_GITHUB_APP_ACCOUNTS
  value: {{ include "muster.githubAccounts" . | quote }}
{{- include "muster.env" (dict "name" "MUSTER_GITHUB_APP_ACCESS" "value" $v.githubApp.access) }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_GITHUB_APP_PRIVATE_KEY" "existing" (required "githubApp.existingSecret is required (the App's PEM, key MUSTER_GITHUB_APP_PRIVATE_KEY)" $v.githubApp.existingSecret) "ctx" .) }}
{{- end }}
{{- if $v.openrouter.enabled }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_AGENT_OPENROUTER_KEY" "existing" (required "openrouter.existingSecret is required (key MUSTER_AGENT_OPENROUTER_KEY)" $v.openrouter.existingSecret) "ctx" .) }}
{{- end }}
{{- if $v.push.enabled }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_VAPID_PUBLIC_KEY" "existing" $v.push.existingSecret "ctx" .) }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_VAPID_PRIVATE_KEY" "existing" $v.push.existingSecret "ctx" .) }}
- name: MUSTER_VAPID_SUBJECT
  value: {{ required "push.subject is required with push.enabled (mailto:<address> or an https URL)" $v.push.subject | quote }}
{{- end }}
{{- if $v.attachments.enabled }}
{{- if $v.attachments.endpoint }}
- name: MUSTER_S3_ENDPOINT
  value: {{ $v.attachments.endpoint | quote }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_S3_ACCESS_KEY" "existing" (required "attachments.existingSecret is required with your own attachments.endpoint" $v.attachments.existingSecret) "ctx" .) }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_S3_SECRET_KEY" "existing" $v.attachments.existingSecret "ctx" .) }}
{{- else }}
- name: MUSTER_S3_ENDPOINT
  value: {{ printf "http://%s-s3:9000" $full | quote }}
- name: MUSTER_S3_ACCESS_KEY
  value: muster
{{ include "muster.secretEnv" (dict "name" "MUSTER_S3_SECRET_KEY" "existing" $v.attachments.existingSecret "ctx" .) }}
{{- end }}
- name: MUSTER_S3_BUCKET
  value: {{ $v.attachments.bucket | quote }}
{{- include "muster.env" (dict "name" "MUSTER_S3_REGION" "value" $v.attachments.region) }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_UPLOAD_SIGNING_KEY" "existing" $v.attachments.signingKeySecret "ctx" .) }}
{{- include "muster.env" (dict "name" "MUSTER_UPLOAD_MAX_BYTES" "value" $v.attachments.maxBytes) }}
{{- end }}
{{- if $v.dictation.enabled }}
- name: MUSTER_STT_URL
  value: {{ default (printf "http://%s-stt:8000" $full) $v.dictation.url | quote }}
{{- if $v.dictation.url }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_STT_TOKEN" "existing" (required "dictation.existingSecret is required with your own dictation.url (key MUSTER_STT_TOKEN)" $v.dictation.existingSecret) "ctx" .) }}
{{- include "muster.env" (dict "name" "MUSTER_STT_MODEL" "value" $v.dictation.model) }}
{{- else }}
{{ include "muster.secretEnv" (dict "name" "MUSTER_STT_TOKEN" "existing" $v.dictation.existingSecret "ctx" .) }}
- name: MUSTER_STT_MODEL
  value: {{ default $v.stt.model $v.dictation.model | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "muster.kvList" -}}
{{- $out := list -}}
{{- range $k := keys . | sortAlpha -}}
{{- $out = append $out (printf "%s=%s" $k (index $ $k)) -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{- define "muster.githubAccounts" -}}
{{- $out := list -}}
{{- range required "githubApp.accounts is required with githubApp.enabled ([{login, id}])" .Values.githubApp.accounts -}}
{{- $out = append $out (printf "%s:%d" .login (int64 .id)) -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{/* Cluster-scoped objects are named after the release AND its namespace, so
two releases of the same name in two namespaces never share one. */}}
{{- define "muster.clusterScopedName" -}}
{{- printf "%s-%s" .Release.Namespace (include "muster.fullname" .) | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "muster.agentNamespaceRole" -}}
{{- printf "%s-agent-namespace" (include "muster.clusterScopedName" .) -}}
{{- end -}}
