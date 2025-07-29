package server

import (
	"fmt"
	"path/filepath"

	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/rs/zerolog"
)

type Server struct {
	config         *Config
	log            *zerolog.Logger
	RESTApiService *RESTApiService
	bpManager      *blueprint.BlueprintManager
}

func NewServer(configFile string) (*Server, error) {
	server := &Server{
		log: log.NewLogger("server"),
	}

	var err error
	server.log.Info().Msgf("Loading server configuration from %s", configFile)
	server.config, err = NewConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	server.log.Info().Msgf("Loading blueprints from directory: %s", server.config.Blueprints.Directory)
	server.bpManager, err = blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:         filepath.Join(server.config.BaseDir, server.config.Blueprints.Directory),
		EnableWatch: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create blueprint manager: %w", err)
	}

	server.RESTApiService, err = NewRESTAPI(server.config.Http, server.bpManager)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST API service: %w", err)
	}

	return server, nil
}
