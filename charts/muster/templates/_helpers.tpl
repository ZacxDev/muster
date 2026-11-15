{{/* Names and labels. */}}
{{- define "muster.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "muster.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "muster.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: muster
app.kubernetes.io/version: {{ include "muster.imageTag" . | trunc 63 | quote }}
{{- end -}}

{{/* The server's selector labels. They are also MUSTER_AGENT_NETPOL_FROM_POD_LABELS:
an agent's NetworkPolicy admits exactly the pods carrying them. */}}
{{- define "muster.selectorLabels" -}}
app.kubernetes.io/name: {{ include "muster.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "muster.componentSelector" -}}
app.kubernetes.io/name: {{ include "muster.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "muster.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "muster.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The image tag: image.tag, else appVersion. The repository's own Chart.yaml
carries appVersion 0.0.0, which is no release: refuse it rather than render a
pull that can never succeed. */}}
{{- define "muster.imageTag" -}}
{{- $t := default .Chart.AppVersion .Values.image.tag -}}
{{- if and (eq $t "0.0.0") (not .Values.image.digest) -}}
{{- fail "image.tag is required: this chart was installed from the repository directory (appVersion 0.0.0). Pass --set image.tag=X.Y.Z (a release), or install the released chart (docs/kubernetes.md)" -}}
{{- end -}}
{{- $t -}}
{{- end -}}

{{- define "muster.image" -}}
{{- $r := .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" $r (include "muster.imageTag" .) .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $r (include "muster.imageTag" .) -}}
{{- end -}}
{{- end -}}

{{- define "muster.generatedSecretName" -}}
{{- printf "%s-generated" (include "muster.fullname" .) -}}
{{- end -}}

{{- define "muster.postgresName" -}}
{{- printf "%s-postgres" (include "muster.fullname" .) -}}
{{- end -}}

{{- define "muster.svcHost" -}}
{{- printf "%s.%s.svc" (include "muster.fullname" .) .Release.Namespace -}}
{{- end -}}

{{/* The connect listener (MUSTER_CONNECT_PORT, MUSTER_AGENT_CONNECT_URL) is
opened with the connector store and only with it: the server refuses a connect
port without the store's key ("the connect listener would serve only
refusals"), and a claude-code agent reaches muster's MCP tools through it. */}}
{{- define "muster.connectOn" -}}
{{- if .Values.connectors.enabled -}}true{{- end -}}
{{- end -}}

{{- define "muster.gotrueOn" -}}
{{- if eq .Values.auth.mode "gotrue" -}}true{{- end -}}
{{- end -}}

{{/* The bundled S3 store is used when attachments are on with no endpoint. */}}
{{- define "muster.s3Bundled" -}}
{{- if and .Values.attachments.enabled (not .Values.attachments.endpoint) -}}true{{- end -}}
{{- end -}}

{{- define "muster.sttBundled" -}}
{{- if and .Values.dictation.enabled (not .Values.dictation.url) -}}true{{- end -}}
{{- end -}}

{{/*
secretRef: an env var read from `existing` if set, else from the generated Secret.
Usage: include "muster.secretEnv" (dict "name" "X" "existing" .Values.foo.existingSecret "key" "X" "ctx" $)
*/}}
{{- define "muster.secretEnv" -}}
- name: {{ .name }}
  valueFrom:
    secretKeyRef:
      name: {{ default (include "muster.generatedSecretName" .ctx) .existing }}
      key: {{ default .name .key }}
{{- end -}}

{{/* A plain env var; skipped when the value is empty. */}}
{{- define "muster.env" -}}
{{- if ne (toString .value) "" }}
- name: {{ .name }}
  value: {{ toString .value | quote }}
{{- end }}
{{- end -}}

{{/*
The generated values, one dict. Each is the value already in the live
Secret (lookup), else a new one. 🔴 THE LIVE SECRET'S KEYS ARE ALL CARRIED,
WHETHER OR NOT A FEATURE STILL NEEDS THEM: turning push or the connector store
off and on again must not mint a new VAPID pair (every subscription dies) or a
new connector key (every stored credential is unreadable).
*/}}
{{- define "muster.generated" -}}
{{- $existing := dict -}}
{{- $live := lookup "v1" "Secret" .Release.Namespace (include "muster.generatedSecretName" .) -}}
{{- if and $live $live.data -}}
{{- range $k, $v := $live.data -}}
{{- $_ := set $existing $k (b64dec $v) -}}
{{- end -}}
{{- end -}}
{{- $out := deepCopy $existing -}}
{{- $want := list -}}
{{- if not .Values.auth.existingSecret -}}
{{- $want = append $want (list "MUSTER_UI_PASSWORD" (randAlphaNum 24)) -}}
{{- $want = append $want (list "MUSTER_HOOK_TOKEN" (randAlphaNum 40)) -}}
{{- end -}}
{{- if and .Values.postgres.enabled (not .Values.postgres.existingSecret) -}}
{{- $want = append $want (list "POSTGRES_PASSWORD" (randAlphaNum 32)) -}}
{{- end -}}
{{- if and .Values.connectors.enabled (not .Values.connectors.existingSecret) -}}
{{- $want = append $want (list "MUSTER_CONNECTOR_ENCRYPTION_KEY" (randBytes 32)) -}}
{{- end -}}
{{- if and .Values.attachments.enabled (not .Values.attachments.signingKeySecret) -}}
{{- $want = append $want (list "MUSTER_UPLOAD_SIGNING_KEY" (randAlphaNum 48)) -}}
{{- end -}}
{{- if and (include "muster.s3Bundled" .) (not .Values.attachments.existingSecret) -}}
{{- $want = append $want (list "MUSTER_S3_SECRET_KEY" (randAlphaNum 40)) -}}
{{- end -}}
{{- if and (include "muster.sttBundled" .) (not .Values.dictation.existingSecret) -}}
{{- $want = append $want (list "MUSTER_STT_TOKEN" (randAlphaNum 40)) -}}
{{- end -}}
{{- if and .Values.gotrue.enabled (not .Values.gotrue.existingSecret) -}}
{{- $want = append $want (list "MUSTER_GOTRUE_JWT_SECRET" (randAlphaNum 48)) -}}
{{- end -}}
{{- if .Values.gotrue.enabled -}}
{{- $want = append $want (list "GOTRUE_DB_PASSWORD" (randAlphaNum 32)) -}}
{{- end -}}
{{- if and .Values.preview.enabled (not .Values.preview.existingSecret) -}}
{{- $want = append $want (list "PREVIEW_SIGNING_KEY" (randAlphaNum 48)) -}}
{{- end -}}
{{- range $want -}}
{{- if not (hasKey $out (index . 0)) -}}
{{- $_ := set $out (index . 0) (index . 1) -}}
{{- end -}}
{{- end -}}
{{- /* Derived, not random: rebuilt from the kept password on every render. */ -}}
{{- if and .Values.postgres.enabled (not .Values.postgres.existingSecret) -}}
{{- $_ := set $out "DATABASE_URL" (printf "postgres://muster:%s@%s:5432/muster?sslmode=disable" (index $out "POSTGRES_PASSWORD") (include "muster.postgresName" .)) -}}
{{- end -}}
{{- if .Values.gotrue.enabled -}}
{{- $_ := set $out "GOTRUE_DB_DATABASE_URL" (printf "postgres://muster_auth:%s@%s:5432/muster_auth?sslmode=disable&search_path=auth" (index $out "GOTRUE_DB_PASSWORD") (include "muster.postgresName" .)) -}}
{{- end -}}
{{- if and .Values.push.enabled (not .Values.push.existingSecret) (not (hasKey $out "MUSTER_VAPID_PRIVATE_KEY")) -}}
{{- $pair := include "muster.vapidPair" . | fromYaml -}}
{{- $_ := set $out "MUSTER_VAPID_PUBLIC_KEY" $pair.public -}}
{{- $_ := set $out "MUSTER_VAPID_PRIVATE_KEY" $pair.private -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
A VAPID keypair (P-256, unpadded base64url, the form `muster-server vapid
generate` prints), cut out of Sprig's ECDSA key. genPrivateKey "ecdsa" is a
P-256 key in SEC1 DER (x509.MarshalECPrivateKey), whose layout for P-256 is
fixed: 7 header bytes, the 32-byte scalar, 18 bytes (the curve OID and the
BIT STRING header ending in 0x04), then the 64-byte point. The header and
the middle are checked here byte for byte, so a Sprig that changed the
curve or the encoding fails the render instead of shipping a pair muster
refuses at boot (it checks that the private key derives the public one).
*/}}
{{- define "muster.vapidPair" -}}
{{- $der := regexReplaceAll "-----[A-Z ]+-----" (genPrivateKey "ecdsa") "" | nospace | b64dec -}}
{{- if or (ne (len $der) 121) (ne (substr 0 7 $der | b64enc) "MHcCAQEEIA==") (ne (substr 39 57 $der | b64enc) "oAoGCCqGSM49AwEHoUQDQgAE") -}}
{{- fail "push: the generated ECDSA key is not the P-256 SEC1 layout this chart cuts the VAPID pair from; set push.existingSecret (muster-server vapid generate)" -}}
{{- end -}}
public: {{ substr 56 121 $der | b64enc | replace "+" "-" | replace "/" "_" | trimAll "=" }}
private: {{ substr 7 39 $der | b64enc | replace "+" "-" | replace "/" "_" | trimAll "=" }}
{{- end -}}
