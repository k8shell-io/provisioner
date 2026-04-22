package config

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/config"
	"github.com/k8shell-io/common/pkg/gapi"
	natsc "github.com/k8shell-io/common/pkg/nats"
)

// Config represents the server configuration
type Config struct {
	TargetNamespace     string                  `yaml:"targetNamespace"`
	ClusterDomain       string                  `yaml:"clusterDomain"`
	DefaultRegistry     DefaultRegistry         `yaml:"defaultRegistry"`
	K8shellCapabilities K8shellCapabilities     `yaml:"k8shellCapabilities"`
	CertManager         CertManagerConfig       `yaml:"certManager"`
	GrpcConfig          gapi.ServerConfig       `yaml:"grpc"`
	Nats                ProvisionerNatsConfig   `yaml:"nats"`
	Identity            gapi.ClientConfig       `yaml:"identity"`
	JWTVerifier         authz.JWTVerifierConfig `yaml:"jwtVerifier"`
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
	APIServerEnabled bool            `yaml:"apiServerEnabled"`
	Shells           k8shelld.Shells `yaml:"shells,omitempty"`
}

// CertManagerConfig represents the cert-manager configuration
type CertManagerConfig struct {
	Enabled     bool       `yaml:"enabled"`
	Issuer      CertIssuer `yaml:"issuer"`
	Duration    string     `yaml:"duration"`
	RenewBefore string     `yaml:"renewBefore"`
}

// CertIssuer represents the certificate issuer configuration
type CertIssuer struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

// DefaultRegistry represents the default container registry configuration.
type DefaultRegistry struct {
	Host     string `yaml:"host"`
	CertCA   string `yaml:"certCA"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Default cluster domain is "cluster.local", but it can be overridden by configuration.
// This is needed for constructing the FQDN of workspace pods
var ClusterDomain string = "cluster.local"

func (r DefaultRegistry) ToValues() map[string]interface{} {
	values := make(map[string]interface{})
	values["host"] = r.Host
	values["certCA"] = r.CertCA
	if r.Username != "" && r.Password != "" {
		values["dockerConfigJson"] = fmt.Sprintf(`{"auths": {"%s": {"username": "%s","password": "%s"}}}`,
			r.Host, r.Username, r.Password)
		values["regcred"] = "regcred"
	}
	return values
}

// Blueprint represents a blueprint configuration
type BlueprintsFileConfig struct {
	Directory string `yaml:"directory" validate:"required"`
}

func NewConfig(configFile string) (*Config, error) {
	var cfg Config

	processor := config.NewDefaultProcessor()
	if err := processor.LoadAndDecode(configFile, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load configuration from '%s': %w", configFile, err)
	}

	if cfg.Nats.KV.ProvisionBucketTTLRaw != "" {
		d, err := time.ParseDuration(cfg.Nats.KV.ProvisionBucketTTLRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid nats.kv.provisionBucketTTL %q: %w", cfg.Nats.KV.ProvisionBucketTTLRaw, err)
		}
		cfg.Nats.KV.ProvisionBucketTTL = d
	}

	if cfg.GrpcConfig.Port == 0 {
		return nil, fmt.Errorf("missing required configuration values: port must be set")
	}

	if cfg.CertManager.Enabled {
		if cfg.CertManager.Issuer.Name == "" || cfg.CertManager.Issuer.Kind == "" {
			return nil, fmt.Errorf("missing required configuration values: certManager.issuer.name and certManager.issuer.kind must be set when certManager.enabled is true")
		}
		if cfg.CertManager.Duration == "" {
			cfg.CertManager.Duration = "24h"
		}

		if cfg.CertManager.RenewBefore == "" {
			cfg.CertManager.RenewBefore = "12h"
		}
	}

	if cfg.ClusterDomain != ClusterDomain {
		cfg.ClusterDomain = ClusterDomain
	}
	ClusterDomain = cfg.ClusterDomain

	cfg.BaseDir = filepath.Dir(configFile)

	method := cfg.JWTVerifier.SigningMethod
	if method == "" {
		return nil, fmt.Errorf("jwtVerifier.signingMethod is required and must be es256 or rs256")
	}
	if method != "es256" && method != "rs256" {
		return nil, fmt.Errorf("jwtVerifier.signingMethod %q is not supported; must be es256 or rs256", method)
	}
	if cfg.JWTVerifier.PublicKeyFile == "" && cfg.JWTVerifier.PrivateKeyFile == "" {
		return nil, fmt.Errorf("jwtVerifier: publicKeyFile or privateKeyFile is required")
	}

	return &cfg, nil
}
