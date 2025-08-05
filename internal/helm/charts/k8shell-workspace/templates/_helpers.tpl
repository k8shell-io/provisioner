{{/* vim: set filetype=mustache: */}}

{{/* labels for helm resources */}}
{{- define "workspace.labels" -}}
k8shell.io/app: k8shell-workspace
k8shell.io/organization: "{{ .Values.__organization__ }}"
k8shell.io/blueprint: "{{ .Values.__blueprint__ }}"
k8shell.io/workspace: "{{ .Values.__workspace__ }}"
k8shell.io/username: "{{ .Values.__username__ }}"
{{- end -}}

{{/* default networkpolicy egress rules */}}
{{- define "default.egress" -}}
- to:
    - podSelector:
        matchLabels:
          app: k8shell-proxy 
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