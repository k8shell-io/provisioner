package workspace

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"text/template"
)

type Values struct {
	Values map[string]interface{}
}

// workspaceConfigTemplate is the template for the k8shelld configuration
const workspaceConfigTemplate = `# k8shelld configuration file
# system settings
system:
  # true to enable profiling
  # if enabled, pprof will be available at localhost:6060
  pprof: false

# workspace user
mainUser:
  username: {{ .Values.__username__ }}
  fullname: {{ .Values.__user__.fullname }}
  uid: {{ .Values.__user__.uid }}
  gid: {{ .Values.__user__.gid }}
  shell: {{ .Values.shell }}
  sudo: {{ .Values.sudo }}

  {{- if .Values.docker.enabled }}
  # supplementary groups
  groups:
    - name: k8shell-docker
      gid: {{ if and (ne .Values.docker.subgid 0) }}{{ add .Values.docker.subgid .Values.docker.groupId -1 }}{{ else }}{{ .Values.docker.groupId }}{{ end }}
  {{- end }}

{{- if .Values.k8shelld.PortForward }}
# port forwarding rules
portForwarding:
{{- range .Values.k8shelld.PortForward }}
  - {{ . }}
{{- end }}
{{- end }}

# reap zombie processes
reapZombies:
  enabled: true

# terminate orphaned processes, i.e. processes that were reparented to init (pid 1)
terminateOrphans:
  enabled: true
  checkInterval: 5
  {{- if .Values.k8shelld.IgnoreOrphans }}
  exclude:
  {{- range .Values.k8shelld.IgnoreOrphans }}
  - {{ . }}
  {{- end }}
  {{- end }}
`

// k8shelldConfig generates the k8shelld configuration YAML
func (w *Workspace) k8shelldConfig(encrypt bool, a1keyHex string, values map[string]interface{}) (string, error) {
	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"ne":  func(a, b int) bool { return a != b },
		"and": func(args ...interface{}) bool {
			for _, arg := range args {
				if !arg.(bool) {
					return false
				}
			}
			return true
		},
	}

	tmpl, err := template.New("config").Funcs(funcMap).Parse(workspaceConfigTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse config template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, Values{Values: values}); err != nil {
		return "", fmt.Errorf("failed to execute config template: %w", err)
	}

	configYAML := buf.String()

	if encrypt && a1keyHex != "" {
		encrypted, err := w.encryptText(a1keyHex, configYAML)
		if err != nil {
			return "", fmt.Errorf("failed to encrypt config: %w", err)
		}
		return encrypted, nil
	}

	return base64.StdEncoding.EncodeToString([]byte(configYAML)), nil
}

// encryptText encrypts plaintext using AES-GCM with the provided key
func (w *Workspace) encryptText(a1keyHex, plaintext string) (string, error) {
	keyBytes, err := hex.DecodeString(a1keyHex)
	if err != nil {
		return "", fmt.Errorf("failed to decode hex key: %w", err)
	}

	if len(keyBytes) != 16 && len(keyBytes) != 24 && len(keyBytes) != 32 {
		return "", fmt.Errorf("AES key must be 16, 24, or 32 bytes long, got %d", len(keyBytes))
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM mode: %w", err)
	}

	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	encryptedData := append(nonce, ciphertext...)
	encodedData := base64.StdEncoding.EncodeToString(encryptedData)

	return fmt.Sprintf("ENC[AES256]%s", encodedData), nil
}
