package server

import (
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/rs/zerolog"
)

type Server struct {
	log *zerolog.Logger
}

func NewServer(configFile string) (*Server, error) {
	server := &Server{
		log: log.NewLogger("server"),
	}

	server.log.Info().Msgf("Loading server configuration from %s", configFile)
	return server, nil
}
