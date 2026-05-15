{{/* vim: set filetype=mustache: */}}

{{/* labels for helm resources */}}
{{- define "workspace.labels" -}}
k8shell.io/workspace: "{{ .Release.Name }}"
{{- end -}}

{{/* labels for helm resources */}}
{{- define "workspace.workspaceLabels" -}}
k8shell.io/k8shelld-version: {{ .Values.__appversion__ }}
k8shell.io/workspace: "{{ .Release.Name }}"
k8shell.io/canonical-id: "{{ .Values.__canonicalid__ }}"
k8shell.io/blueprint: "{{ .Values.__blueprint__ }}"
k8shell.io/username: "{{ .Values.__username__ }}"
k8shell.io/organization: "{{ .Values.__organization__ }}"
{{- if and .Values.network .Values.network.networkPolicyClass }}
k8shell.io/network-policy: "{{ .Values.network.networkPolicyClass }}"
{{- end }}
{{- if and .Values.subdomain .Values.hostname }}
k8shell.io/subdomain: "{{ .Values.subdomain }}"
{{- end }}
{{- if .Values.__jobid__ }}
k8shell.io/job-id: "{{ .Values.__jobid__ }}"
{{- end }}
{{- end -}}

{{/* default networkpolicy ingress rules */}}
{{- define "default.ingress" -}}
- from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.__namespace__ }}
      podSelector:
        matchLabels:
          k8shell.io/app: ssh-proxy
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.__namespace__ }}
      podSelector:
        matchLabels:
          k8shell.io/app: api-server
{{- end -}}

{{/* default networkpolicy egress rules */}}
{{- define "default.egress" -}}
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.__namespace__ }}
      podSelector:
        matchLabels:
          k8shell.io/app: ssh-proxy
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: {{ .Values.__namespace__ }}
      podSelector:
        matchLabels:
          k8shell.io/app: api-server
    {{- range .Values.network.allowEgressToPods }}
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          {{- range $k, $v := . }}
          {{ $k }}: {{ $v | quote }}
          {{- end }}
    {{- end }}
    {{- range $cidr := .Values.network.allowEgressToCIDRs }}
    - ipBlock:
        cidr: {{ $cidr }}
    {{- end }}
    - ipBlock:
        cidr: 0.0.0.0/0
        except:
        - 10.0.0.0/8
        - 192.168.0.0/16
        - 172.16.0.0/12
- to:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: kube-system
      podSelector:
        matchLabels:
          k8s-app: kube-dns
{{- /* kube-apiserver ClusterIP lives in the service CIDR (default 10.96.0.0/12 for kubeadm)
     which is inside 10.0.0.0/8 and therefore excluded by the ipBlock rule above.
     This explicit carve-out restores reachability for kubectl and k8s client calls.
     Adjust if your cluster uses a non-default --service-cluster-ip-range. */}}
- to:
    - ipBlock:
        cidr: 10.96.0.0/12
{{- end -}}

{{/* pvc template */}}
{{- define "pvc-template" -}}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: "pvc-{{ .ctx.Release.Name }}-{{ .pvcPrefix }}{{ .name }}"
  namespace: {{ .ctx.Release.Namespace }} 
  {{- if .storage.claimSpecAnnotations }}
  annotations:
  {{- range $key, $value := .storage.claimSpecAnnotations }}
    {{ $key | quote }}: {{ $value | quote }}
  {{- end }}
  {{- end }}
  labels:
    {{ include "workspace.labels" .ctx | nindent 4 }}
spec:
  {{- toYaml .storage.claimSpec | nindent 2 }}
{{- end }}

{{/*
Build full image name with registry if needed
Uses regex to detect registry hostname in image name
*/}}
{{- define "workspace.imageWithRegistry" -}}
{{- $image := .image | toString -}}
{{- $registry := .registry | toString -}}

{{/* Use regex to check if image starts with a registry (hostname with . or :) */}}
{{- if regexMatch "^[^/]*[.:].*/" $image -}}
  {{/* Image already has a registry */}}
  {{- $image -}}
{{- else if $registry -}}
  {{/* No registry detected, prepend the provided registry */}}
  {{- $registry -}}/{{- $image -}}
{{- else -}}
  {{/* No registry configured, use image as-is */}}
  {{- $image -}}
{{- end -}}
{{- end }}

{{/*
Convenience helper for k8shelld image
*/}}
{{- define "workspace.k8shelldImage" -}}
{{- include "workspace.imageWithRegistry" (dict "image" .Values.k8shelld.image "registry" .Values.__registry__.host) -}}
{{- end }}

{{/*
Convenience helper for main container image  
*/}}
{{- define "workspace.mainImage" -}}
{{- include "workspace.imageWithRegistry" (dict "image" .Values.image "registry" .Values.__registry__.host) -}}
{{- end }}

{{- /*
Render storages config from a map like .Values.storages.

Usage:
  storages:{{ include "workspace.storages" (dict "storages" .Values.storages) | nindent 6 }}
*/ -}}
{{- define "workspace.storages" -}}
{{- $storages := list -}}
{{- range $name, $s := (.storages | default dict) }}
  {{- if and $s (kindIs "map" $s) ($s.enabled | default false) }}
    {{- $ro := false -}}
    {{- if and (kindIs "map" $s) (hasKey $s "readonly") -}}
      {{- $ro = ($s.readonly | default false) -}}
    {{- end -}}
    {{- $storages = append $storages (dict
        "name" $name
        "path" (required (printf "storages.%s.path is required" $name) $s.path)
        "size" (required (printf "storages.%s.size is required" $name) ($s.size | toString))
        "readonly" $ro
      ) -}}
  {{- end }}
{{- end }}
{{- toYaml $storages -}}
{{- end -}}

{{/*
Emit chown shell commands for storages in the init container.
For writable non-shared storages: uses fsOwnerUid/fsOwnerGid when explicitly set (non-zero),
otherwise falls back to the workspace user's uid/gid.
Expects: dict "storages" <storages-map> "user" <__user__-map>
*/}}
{{- define "workspace.storage.chownCommands" -}}
{{- $user := .user | default dict -}}
{{- range $name, $s := .storages -}}
{{- if and $s (kindIs "map" $s) ($s.enabled | default false) (not ($s.readonly | default false)) (ne ($s.type | default "local") "shared") -}}
{{- $uidExplicit := hasKey $s "fsOwnerUid" -}}
{{- $gidExplicit := hasKey $s "fsOwnerGid" -}}
{{- $uid := 0 | int -}}
{{- $gid := 0 | int -}}
{{- if $uidExplicit -}}{{- $uid = $s.fsOwnerUid | int -}}{{- else -}}{{- $uid = $user.uid | default 0 | int -}}{{- end -}}
{{- if $gidExplicit -}}{{- $gid = $s.fsOwnerGid | int -}}{{- else -}}{{- $gid = $user.gid | default 0 | int -}}{{- end -}}
{{- printf "chown %d:%d %s\n" $uid $gid $s.path -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Emit volumeMount entries for the init container chown step.
Expects: dict "storages" <storages-map> "prefix" <volume-name-prefix> "user" <__user__-map>
*/}}
{{- define "workspace.storage.chownVolumeMounts" -}}
{{- $prefix := .prefix -}}
{{- $user := .user | default dict -}}
{{- range $name, $s := .storages -}}
{{- if and $s (kindIs "map" $s) ($s.enabled | default false) (not ($s.readonly | default false)) (ne ($s.type | default "local") "shared") }}
- name: storage-{{ $prefix }}{{ $name }}
  mountPath: {{ $s.path }}
{{- end -}}
{{- end -}}
{{- end -}}