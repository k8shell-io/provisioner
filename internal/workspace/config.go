package workspace

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"text/template"
	"time"
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

// GenerateSelfSignedCert generates a self-signed TLS certificate and private key
func (w *Workspace) generateSelfSignedCert(hostname string) (keyPEM, certPEM string, err error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("failed to generate RSA private key: %w", err)
	}

	privateKeyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	template := x509.Certificate{
		SerialNumber: big.NewInt(0).SetBytes(make([]byte, 20)), // Random serial number
		Subject: pkix.Name{
			Country:      []string{w.blueprint.K8shelld.Cert.Country},
			Province:     []string{w.blueprint.K8shelld.Cert.State},
			Locality:     []string{w.blueprint.K8shelld.Cert.Locality},
			Organization: []string{w.blueprint.K8shelld.Cert.Organization},
			CommonName:   w.blueprint.K8shelld.Cert.CommonName,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", fmt.Errorf("failed to generate serial number: %w", err)
	}
	template.SerialNumber = serialNumber

	if hostname != "" && w.Namespace() != "" {
		template.DNSNames = []string{fmt.Sprintf("%s.%s", hostname, w.Namespace())}
	}

	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return "", "", fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: certBytes,
	}))

	keyPEM = string(privateKeyPEM)

	return keyPEM, certPEM, nil
}

// GenerateAccessKeys generates random access keys similar to Python's secrets.token_hex
func (w *Workspace) generateAccessKeys() (a1keyB64, a2keyB64 string, err error) {
	a1Bytes := make([]byte, 16)
	if _, err := rand.Read(a1Bytes); err != nil {
		return "", "", fmt.Errorf("failed to generate a1key: %w", err)
	}

	a2Bytes := make([]byte, 16)
	if _, err := rand.Read(a2Bytes); err != nil {
		return "", "", fmt.Errorf("failed to generate a2key: %w", err)
	}

	a1key := hex.EncodeToString(a1Bytes)
	a2key := hex.EncodeToString(a2Bytes)
	a1keyB64 = base64.StdEncoding.EncodeToString([]byte(a1key))
	a2keyB64 = base64.StdEncoding.EncodeToString([]byte(a2key))

	return a1keyB64, a2keyB64, nil
}
