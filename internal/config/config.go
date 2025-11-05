package config

import (
	"fmt"
	"path/filepath"

	"github.com/k8shell-io/common/pkg/config"
	"github.com/k8shell-io/common/pkg/gapi"
)

// Config represents the server configuration
type Config struct {
	TargetNamespace string               `yaml:"targetNamespace"`
	DefaultRegistry DefaultRegistry      `yaml:"defaultRegistry"`
	CertManager     CertManagerConfig    `yaml:"certManager"`
	GrpcConfig      gapi.ServerConfig    `yaml:"grpc"`
	Identity        gapi.ClientConfig    `yaml:"identity"`
	Blueprints      BlueprintsFileConfig `yaml:"blueprints"`
	BaseDir         string               `yaml:"baseDir"`
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

	cfg.BaseDir = filepath.Dir(configFile)
	return &cfg, nil
}
