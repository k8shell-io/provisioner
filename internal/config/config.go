// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package config defines the server configuration structures and the loader
// that parses, validates, and post-processes the YAML configuration file
// supplied to the provisioner on startup.
package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/config"
	"github.com/k8shell-io/common/pkg/gapi"
	natsc "github.com/k8shell-io/common/pkg/nats"
	"github.com/k8shell-io/common/pkg/validator"
)

// Config represents the server configuration
type Config struct {
	TargetNamespace     string                  `yaml:"targetNamespace" validate:"required"`
	InjectNamespaces    []string                `yaml:"injectNamespaces"`
	ClusterDomain       string                  `yaml:"clusterDomain"`
	DefaultRegistry     DefaultRegistry         `yaml:"defaultRegistry"`
	PrivateRegistry     PrivateRegistry         `yaml:"privateRegistry"`
	K8shellCapabilities K8shellCapabilities     `yaml:"k8shellCapabilities"`
	CertManager         CertManagerConfig       `yaml:"certManager"`
	GrpcConfig          gapi.ServerConfig       `yaml:"grpc" validate:"required"`
	Nats                ProvisionerNatsConfig   `yaml:"nats"`
	Identity            gapi.ClientConfig       `yaml:"identity"`
	Authz               gapi.ClientConfig       `yaml:"authz"`
	JWTVerifier         authz.JWTVerifierConfig `yaml:"jwtVerifier" validate:"required"`
	Blueprints          BlueprintsFileConfig    `yaml:"blueprints"`
	BaseDir             string                  `yaml:"baseDir"`
}

// ProvisionerNatsConfig represents the NATS configuration for the provisioner
type ProvisionerNatsConfig struct {
	natsc.NATSClientConfig `yaml:",inline" mapstructure:",squash"`
	KV                     JobsKVConfig `yaml:"kv" mapstructure:"kv"`
}

// JobsKVConfig represents the configuration for the NATS Key-Value store used for provisioning jobs
type JobsKVConfig struct {
	ProvisionBucketTTL    time.Duration `yaml:"-"`
	ProvisionBucketTTLRaw string        `yaml:"provisionBucketTTL"`
}

// K8shellCapabilities represents the capabilities of the k8shell environment
type K8shellCapabilities struct {
	APIServerEnabled bool             `yaml:"apiServerEnabled"`
	SaToken          k8shelld.SaToken `yaml:"saToken,omitempty"`
	Shells           k8shelld.Shells  `yaml:"shells,omitempty"`
}

// CertManagerConfig represents the cert-manager configuration
type CertManagerConfig struct {
	Enabled     bool       `yaml:"enabled"`
	Issuer      CertIssuer `yaml:"issuer" validate:"required_if=Enabled true"`
	Duration    string     `yaml:"duration"`
	RenewBefore string     `yaml:"renewBefore"`
}

// CertIssuer represents the certificate issuer configuration
type CertIssuer struct {
	Name string `yaml:"name" validate:"required"`
	Kind string `yaml:"kind" validate:"required,oneof=ClusterIssuer Issuer"`
}

// DefaultRegistry is the default container registry host used to prefix image names.
type DefaultRegistry struct {
	Host string `yaml:"host"`
}

// PrivateRegistry holds credentials for a private registry used to create imagePullSecrets
// and a registry CA configmap. It is optional — if not set, no regcred is created.
type PrivateRegistry struct {
	Host     string `yaml:"host"`
	CertCA   string `yaml:"certCA"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Default cluster domain is "cluster.local", but it can be overridden by configuration.
// This is needed for constructing the FQDN of workspace pods
var ClusterDomain string = "cluster.local"

// ToValues returns the registry fields as a Helm values map.
func (r DefaultRegistry) ToValues() map[string]interface{} {
	return map[string]interface{}{
		"host": r.Host,
	}
}

// ToValues returns the private registry fields as a Helm values map, including
// the base64-encoded dockerConfigJson when credentials are configured.
func (r PrivateRegistry) ToValues() map[string]interface{} {
	values := map[string]interface{}{
		"certCA": r.CertCA,
	}
	if r.Username != "" && r.Password != "" {
		values["dockerConfigJson"] = fmt.Sprintf(`{"auths": {"%s": {"username": "%s","password": "%s"}}}`,
			r.Host, r.Username, r.Password)
		values["regcred"] = "regcred"
	}
	return values
}

// Blueprint represents a blueprint configuration
type BlueprintsFileConfig struct {
	Directory              string `yaml:"directory" validate:"required"`
	DefaultCustomBlueprint string `yaml:"defaultCustomBlueprint"`
}

// NewConfig loads and fully validates the server configuration from configFile.
// Post-load steps include: parsing duration strings, applying defaults for
// cert-manager, validating the JWT signing method, deduplicating injection
// namespaces, and resolving relative paths against the config file directory.
func NewConfig(configFile string) (*Config, error) {
	var cfg Config

	processor := config.NewDefaultProcessor()
	if err := processor.LoadAndDecode(configFile, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load configuration from '%s': %w", configFile, err)
	}

	// Structural validation via common validator
	if errs := validator.NewValidator(cfg); !errs.IsValid() {
		return nil, fmt.Errorf("invalid configuration:\n%s", errs.ErrorMessages())
	}

	// Post-load derived fields and cross-field checks
	if cfg.Nats.KV.ProvisionBucketTTLRaw != "" {
		d, err := time.ParseDuration(cfg.Nats.KV.ProvisionBucketTTLRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid nats.kv.provisionBucketTTL %q: %w", cfg.Nats.KV.ProvisionBucketTTLRaw, err)
		}
		cfg.Nats.KV.ProvisionBucketTTL = d
	}

	if cfg.CertManager.Enabled {
		if cfg.CertManager.Duration == "" {
			cfg.CertManager.Duration = "24h"
		}
		if cfg.CertManager.RenewBefore == "" {
			cfg.CertManager.RenewBefore = "12h"
		}
	}

	method := cfg.JWTVerifier.SigningMethod
	if method != "es256" && method != "rs256" {
		return nil, fmt.Errorf("jwtVerifier.signingMethod %q is not supported; must be es256 or rs256", method)
	}
	if cfg.JWTVerifier.PublicKeyFile == "" && cfg.JWTVerifier.PrivateKeyFile == "" {
		return nil, fmt.Errorf("jwtVerifier: publicKeyFile or privateKeyFile is required")
	}

	if cfg.ClusterDomain == "" {
		cfg.ClusterDomain = ClusterDomain
	}
	ClusterDomain = cfg.ClusterDomain

	for i := range cfg.InjectNamespaces {
		ns := strings.TrimSpace(cfg.InjectNamespaces[i])
		if ns == "" {
			return nil, fmt.Errorf("injectNamespaces[%d] must not be empty", i)
		}
		cfg.InjectNamespaces[i] = ns
	}

	cfg.BaseDir = filepath.Dir(configFile)

	injectedNamespaces := make([]string, 0, len(cfg.InjectNamespaces))
	seenNamespaces := make(map[string]struct{}, len(cfg.InjectNamespaces))
	for _, ns := range cfg.InjectNamespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if ns == "*" {
			injectedNamespaces = []string{"*"}
			break
		}
		if _, exists := seenNamespaces[ns]; exists {
			continue
		}
		seenNamespaces[ns] = struct{}{}
		injectedNamespaces = append(injectedNamespaces, ns)
	}
	cfg.InjectNamespaces = injectedNamespaces

	return &cfg, nil
}

// AllowsInjectionNamespace returns true when the namespace is permitted by
// injectNamespaces. A literal "*" allows all namespaces.
// Returns false when injectNamespaces is empty, effectively disabling injection.
func (c *Config) AllowsInjectionNamespace(namespace string) bool {
	if c == nil || namespace == "" || len(c.InjectNamespaces) == 0 {
		return false
	}
	for _, ns := range c.InjectNamespaces {
		if ns == "*" || ns == namespace {
			return true
		}
	}
	return false
}

// IsClusterWideInjectionEnabled returns true if injectNamespaces includes "*".
func (c *Config) IsClusterWideInjectionEnabled() bool {
	if c == nil {
		return false
	}
	for _, ns := range c.InjectNamespaces {
		if ns == "*" {
			return true
		}
	}
	return false
}
