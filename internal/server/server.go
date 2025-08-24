package server

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	log "github.com/k8shell-io/common/logger"
	"github.com/k8shell-io/common/models"
	identity "github.com/k8shell-io/identity/pkg/client"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
)

type Server struct {
	config         *config.Config
	log            *zerolog.Logger
	Identity       *identity.Client
	RESTApiService *RESTApiService
	bpManager      *blueprint.BlueprintManager
	helm           *helm.Client
}

func NewServer(configFile string) (*Server, error) {
	server := &Server{
		log: log.NewLogger("server"),
	}

	var err error
	server.log.Info().Msgf("Loading server configuration from %s", configFile)
	server.config, err = config.NewConfig(configFile)
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

	server.log.Info().Msgf("Creating identity client with base URL: %s", server.config.Identity.BaseURL)
	server.Identity = identity.NewClient(identity.Config{
		BaseURL: server.config.Identity.BaseURL,
		APIKey:  server.config.Identity.APIKey,
		Timeout: int(time.Duration(server.config.Identity.Timeout) * time.Millisecond),
	})

	server.log.Info().Msg("Creating REST API service")
	server.RESTApiService, err = NewRESTAPI(server)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST API service: %w", err)
	}

	server.log.Info().Msg("Creating Helm client")
	server.helm, err = helm.NewClient(server.config.TargetNamespace, server.config.DefaultRegistry)
	if err != nil {
		return nil, fmt.Errorf("failed to create Helm client: %w", err)
	}

	server.log.Info().Msgf("Ensuring workspace base, namespace %s", server.config.TargetNamespace)
	err = server.helm.EnsureBase(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to ensure base namespace: %w", err)
	}

	return server, nil
}

func (s *Server) GetBlueprintScope(ctx context.Context, username string, repoName string,
	repoOwner string) (*blueprint.BlueprintScope, error) {
	s.log.Debug().Msgf("Getting blueprint scope for user: %s, repo: %s, owner: %s",
		username, repoName, repoOwner)
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
		User: user,
		Repo: models.Repo{
			Name:  repo,
			Owner: owner,
		},
	}
	return scope, nil
}
