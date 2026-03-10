{{/* vim: set filetype=mustache: */}}

{{/* labels for helm resources */}}
{{- define "workspace.labels" -}}
k8shell.io/app: k8shell-workspace
k8shell.io/workspace: "{{ .Values.__workspace__ }}"
{{- end -}}

{{/* labels for helm resources */}}
{{- define "workspace.workspaceLabels" -}}
k8shell.io/app: k8shell-workspace
app.kubernetes.io/version: {{ .Values.__appversion__ }}
k8shell.io/workspace: "{{ .Values.__workspace__ }}"
k8shell.io/blueprint: "{{ .Values.__blueprint__ }}"
{{- if and .Values.__repoowner__ .Values.__reponame__ }}
k8shell.io/repo-owner: "{{ .Values.__repoowner__ }}"
k8shell.io/repo-name: "{{ .Values.__reponame__ }}"
{{- end }}
{{- if .Values.__reporef__ }}
k8shell.io/repo-ref: "{{ .Values.__reporef__ }}"
{{- end }}
k8shell.io/username: "{{ .Values.__username__ }}"
k8shell.io/organization: "{{ .Values.__organization__ }}"
k8shell.io/userstr: "{{ .Values.__userstr__ }}"
k8shell.io/network-policy: "{{ .Values.network.networkPolicy }}"
{{- if and .Values.subdomain .Values.hostname }}
k8shell.io/subdomain: {{ .Values.subdomain }}
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
    - podSelector:
        matchLabels:
          type: backend
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
{{- end -}}

{{/* pvc template */}}
{{- define "pvc-template" -}}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: "pvc-{{ .ctx.Values.__workspace__ }}-{{ .pvcPrefix }}{{ .name }}"
  namespace: {{ .ctx.Release.Namespace }} 
  {{- if .storage.annotations }}
  annotations:
  {{- range $key, $value := .storage.annotations }}
    {{ $key | quote }}: {{ $value | quote }}
  {{- end }}
  {{- end }}
  labels:
    {{ include "workspace.labels" .ctx | nindent 4 }}
spec:
  {{ if .storage.storageClass }}
  storageClassName: {{ .storage.storageClass }}
  {{ end }}
  accessModes:
    - ReadWriteMany
  resources:
    requests:
      storage: {{ .storage.size }}
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
{{- else -}}
  {{/* No registry detected, prepend the provided registry */}}
  {{- $registry -}}/{{- $image -}}
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