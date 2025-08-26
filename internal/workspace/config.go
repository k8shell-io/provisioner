package workspace

import (
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
	"time"
)

type Values struct {
	Values map[string]interface{}
}

// encryptText encrypts plaintext using AES-GCM with the provided key
// This matches the Python implementation format: nonce + tag + ciphertext
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

	if len(ciphertext) < 16 {
		return "", fmt.Errorf("invalid ciphertext length")
	}

	actualCiphertext := ciphertext[:len(ciphertext)-16]
	tag := ciphertext[len(ciphertext)-16:]

	encryptedData := make([]byte, 0, len(nonce)+len(tag)+len(actualCiphertext))
	encryptedData = append(encryptedData, nonce...)
	encryptedData = append(encryptedData, tag...)
	encryptedData = append(encryptedData, actualCiphertext...)

	encodedData := base64.StdEncoding.EncodeToString(encryptedData)

	return fmt.Sprintf("ENC[AES256]%s", encodedData), nil
}

// GenerateKeyCert generates a self-signed TLS certificate and private key
func (w *Workspace) generateKeyCert() (keyPEM, certPEM string, err error) {
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
	template.DNSNames = []string{fmt.Sprintf("%s.%s", w.Name(), w.client.TargetNamespace())}

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

// GenerateAccessKeys generates random access keys
func (w *Workspace) generateAccessKey() (a1keyB64 string, err error) {
	a1Bytes := make([]byte, 16)
	if _, err := rand.Read(a1Bytes); err != nil {
		return "", fmt.Errorf("failed to generate a1key: %w", err)
	}

	a1key := hex.EncodeToString(a1Bytes)

	return a1key, nil
}
