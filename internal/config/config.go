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
	Http            HttpConfig           `yaml:"http"`
	Identity        IdentityConfig       `yaml:"identity"`
	Session         gapi.ClientConfig    `yaml:"session"`
	Blueprints      BlueprintsFileConfig `yaml:"blueprints"`
	BaseDir         string               `yaml:"baseDir"`
}

// HttpConfig represents the HTTP server configuration.
type HttpConfig struct {
	Port   int    `yaml:"port"`
	APIKey string `yaml:"APIKey"`
}

// IdentityConfig represents the identity service configuration.
type IdentityConfig struct {
	BaseURL string `yaml:"baseURL"`
	APIKey  string `yaml:"APIKey"`
	Timeout int    `yaml:"timeout"`
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

	if cfg.Http.Port == 0 || cfg.Http.APIKey == "" {
		return nil, fmt.Errorf("missing required configuration values: port and APIKey must be set")
	}

	cfg.BaseDir = filepath.Dir(configFile)
	return &cfg, nil
}
