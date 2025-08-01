package server

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	identity "github.com/k8shell-io/identity/pkg/client"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/log"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/rs/zerolog"
)

type Server struct {
	config         *Config
	log            *zerolog.Logger
	Identity       *identity.Client
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

	server.Identity = identity.New(identity.Config{
		BaseURL: server.config.Identity.BaseURL,
		APIKey:  server.config.Identity.APIKey,
		Timeout: time.Duration(server.config.Identity.Timeout) * time.Millisecond,
	})

	server.RESTApiService, err = NewRESTAPI(server)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST API service: %w", err)
	}

	return server, nil
}

func (s *Server) GetBlueprintScope(ctx context.Context, username string, repoName string,
	repoOwner string) (*blueprint.BlueprintScope, error) {
	user, err := s.Identity.GetUser(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	var repo = "noreponame"
	var owner = "norepoowner"
	if repoName != "" && repoOwner != "" {
		repo = repoName
		owner = repoOwner
	}

	scope := &blueprint.BlueprintScope{
		User: *user,
		Repo: models.Repo{
			Name:  repo,
			Owner: owner,
		},
	}
	return scope, nil
}
