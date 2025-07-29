package server

import (
	"fmt"

	"github.com/k8shell-io/yaml-config/pkg/yamlconfig"
)

// Config represents the server configuration
type Config struct {
	Http       HttpConfig           `yaml:"http"`
	Blueprints BlueprintsFileConfig `yaml:"blueprints"`
}

// HttpConfig represents the HTTP server configuration.
type HttpConfig struct {
	Port   int    `yaml:"port"`
	APIKey string `yaml:"APIKey"`
}

// Blueprint represents a blueprint configuration
type BlueprintsFileConfig struct {
	Directory string `yaml:"directory" validate:"required"`
}

func NewConfig(configFile string) (*Config, error) {
	var config Config
	err := yamlconfig.LoadConfig(configFile, &config)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration from '%s': %w", configFile, err)
	}

	if config.Http.Port == 0 || config.Http.APIKey == "" {
		return nil, fmt.Errorf("missing required configuration values: port and APIKey must be set")
	}

	return &config, nil

}
