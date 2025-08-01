package server

import (
	"fmt"
	"path/filepath"

	"github.com/k8shell-io/yaml-config/pkg/yamlconfig"
)

// Config represents the server configuration
type Config struct {
	Http       HttpConfig           `yaml:"http"`
	Identity   IdentityConfig       `yaml:"identity"`
	Blueprints BlueprintsFileConfig `yaml:"blueprints"`
	BaseDir    string               `yaml:"baseDir"`
}

// HttpConfig represents the HTTP server configuration.
type HttpConfig struct {
	Port   int    `yaml:"port"`
	APIKey string `yaml:"APIKey"`
}

type IdentityConfig struct {
	BaseURL string `yaml:"baseURL"`
	APIKey  string `yaml:"APIKey"`
	Timeout int    `yaml:"timeout"`
}

// Blueprint represents a blueprint configuration
type BlueprintsFileConfig struct {
	Directory string `yaml:"directory" validate:"required"`
}

func NewConfig(configFile string) (*Config, error) {
	var config Config

	processor := yamlconfig.NewDefaultProcessor()
	if err := processor.LoadAndDecode(configFile, &config); err != nil {
		return nil, fmt.Errorf("failed to load configuration from '%s': %w", configFile, err)
	}

	if config.Http.Port == 0 || config.Http.APIKey == "" {
		return nil, fmt.Errorf("missing required configuration values: port and APIKey must be set")
	}

	config.BaseDir = filepath.Dir(configFile)
	return &config, nil
}
