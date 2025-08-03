package workspace

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	identity "github.com/k8shell-io/identity/pkg/models"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/k8shell-io/provisioner/pkg/models"
	"gopkg.in/yaml.v3"
)

// Workspace represents a workspace with Helm client
type Workspace struct {
	client    *helm.Client
	namespace string
	blueprint *models.Blueprint
	user      *identity.User
}

// NewWorkspace creates a new workspace with the specified Helm chart
func NewWorkspace(blueprint *models.Blueprint, user *identity.User,
	client *helm.Client) (*Workspace, error) {
	return &Workspace{
		client:    client,
		blueprint: blueprint,
		user:      user,
		namespace: "k8shell-dev-testing",
	}, nil
}

func (w *Workspace) Name() string {
	return w.blueprint.Name + "-" + w.user.Username
}

func (w *Workspace) Values() (map[string]interface{}, error) {
	values, err := w.blueprint.Values()
	if err != nil {
		return nil, err
	}

	tls, key, err := w.generateSelfSignedCert(w.Name())
	if err != nil {
		return nil, fmt.Errorf("failed to generate TLS certificate: %w", err)
	}

	a1key, a2key, err := w.generateAccessKeys()
	if err != nil {
		return nil, fmt.Errorf("failed to generate access keys: %w", err)
	}

	userValues, err := ToMap(w.user)
	if err != nil {
		return nil, fmt.Errorf("failed to convert user to map: %w", err)
	}

	values["__user__"] = userValues
	values["__namespace__"] = w.namespace
	values["__workspace__"] = w.Name()
	values["__blueprint__"] = w.blueprint.Name
	values["__username__"] = w.user.Username
	values["__organization__"] = "org1"
	values["__tlscrt__"] = tls
	values["__tlskey__"] = key
	values["__a1key__"] = a1key
	values["__a2key__"] = a2key

	config, err := w.k8shelldConfig(w.blueprint.K8shelld.EncryptConfig, a1key, values)
	if err != nil {
		return nil, fmt.Errorf("failed to generate k8shelld config YAML: %w", err)
	}

	values["__k8shelldconfig__"] = config

	return values, nil
}

func (w *Workspace) Template(ctx context.Context) (string, error) {
	values, err := w.Values()
	if err != nil {
		return "", err
	}
	out, err := w.client.Template(ctx, helm.WORKSPACE_CHART_NAME, helm.InstallOptions{
		ReleaseName: w.blueprint.Name,
		Namespace:   w.namespace,
		Values:      values,
		Wait:        false,
		Timeout:     20,
	})
	if err != nil {
		return "", err
	}
	return out, nil
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

	if hostname != "" && w.namespace != "" {
		template.DNSNames = []string{fmt.Sprintf("%s.%s", hostname, w.namespace)}
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

// ToMap converts any struct to a map[string]interface{} representation
func ToMap(b any) (map[string]interface{}, error) {
	yamlBytes, err := yaml.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal struct to YAML: %w", err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(yamlBytes, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML to map: %w", err)
	}

	return values, nil
}
