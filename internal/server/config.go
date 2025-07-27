package server

import (
	"fmt"

	"github.com/k8shell-io/yaml-config/pkg/yamlconfig"
)

type Config struct {
	Http HttpConfig `yaml:"http"`
}

func LoadConfig(configFile string) (*Config, error) {
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
