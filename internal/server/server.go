package server

import (
	"fmt"

	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/rs/zerolog"
)

type Server struct {
	config         *Config
	log            *zerolog.Logger
	RESTApiService *RESTApiService
}

func NewServer(configFile string) (*Server, error) {
	server := &Server{
		log: log.NewLogger("server"),
	}

	var err error
	server.config, err = LoadConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load configuration: %w", err)
	}

	server.RESTApiService, err = NewRESTAPI(server.config.Http)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST API service: %w", err)
	}

	server.log.Info().Msgf("Loading server configuration from %s", configFile)
	return server, nil
}
