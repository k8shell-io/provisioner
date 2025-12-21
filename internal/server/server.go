package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/k8shell-io/common/pkg/gapi"
	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/common/pkg/models"
	identity "github.com/k8shell-io/identity/pkg/api"
	"github.com/k8shell-io/identity/pkg/api/identitypb"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/k8shell-io/provisioner/pkg/api/provisionerpb"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

type Server struct {
	config    *config.Config
	log       *zerolog.Logger
	Identity  *identity.Client
	grpc      *gapi.Server
	bpManager *blueprint.BlueprintManager
	helm      *helm.Client
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

	var blueprintDir string
	if filepath.IsAbs(server.config.Blueprints.Directory) {
		blueprintDir = server.config.Blueprints.Directory
	} else {
		blueprintDir = filepath.Join(server.config.BaseDir, server.config.Blueprints.Directory)
	}

	server.log.Info().Msgf("Loading blueprints from directory: %s", blueprintDir)
	server.bpManager, err = blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:         blueprintDir,
		EnableWatch: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create blueprint manager: %w", err)
	}

	server.Identity, err = identity.NewClient(server.config.Identity)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity client: %w", err)
	}

	server.log.Info().Msg("Creating Helm client")
	server.helm, err = helm.NewClient(server.config.TargetNamespace, server.config.DefaultRegistry)
	if err != nil {
		return nil, fmt.Errorf("failed to create Helm client: %w", err)
	}

	server.log.Info().Msg("Creating gRPC service")
	server.grpc, err = gapi.NewServer(&server.config.GrpcConfig, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC service: %w", err)
	}

	server.grpc.RegisterService(func(s *grpc.Server) error {
		provisionerpb.RegisterProvisionerServiceServer(s, NewProvisionerService(server))
		return nil
	})

	models.SetIssueRepoRefResolver(server)

	// server.log.Info().Msgf("Ensuring workspace base, namespace %s", server.config.TargetNamespace)
	// err = server.helm.EnsureBase(context.Background())
	// if err != nil {
	// 	return nil, fmt.Errorf("failed to ensure base namespace: %w", err)
	// }

	return server, nil
}

// ResolveIssueRef resolves an issue number to a git reference
// Implements models.IssueRepoRefResolver
func (s Server) ResolveIssueRepoRef(username string, repoOwner, repoName string, issueNumber int) (string, error) {
	ctx := context.Background()
	resp, err := s.Identity.ResolveRepoIssueToRef(ctx, &identitypb.RepoIssueRequest{
		Username:    username,
		RepoOwner:   repoOwner,
		RepoName:    repoName,
		IssueNumber: int32(issueNumber),
	})
	if err != nil {
		return "", fmt.Errorf("failed to resolve issue to ref: %w", err)
	}
	return resp.RepoRef, nil
}

func (s *Server) GetBlueprintScope(blueprintName string, user *models.User,
	bpMetadata *models.BlueprintMetadata, workspaceName string) (*blueprint.BlueprintScope, error) {

	if blueprintName == "" {
		return nil, fmt.Errorf("blueprint name is required to create scope")
	}

	var repoName = "noreponame"
	var repoRef = "noreporef"
	var ownerName = "norepoowner"
	var repoAddress = "noaddress"

	if bpMetadata != nil {
		if bpMetadata.RepoName != "" {
			repoName = bpMetadata.RepoName
		}
		if bpMetadata.RepoOwner != "" {
			ownerName = bpMetadata.RepoOwner
		}
		if bpMetadata.RepoAddress != "" {
			repoAddress = bpMetadata.RepoAddress
		}
		if bpMetadata.RepoRef != "" {
			repoRef = bpMetadata.RepoRef
		}
	}

	s.log.Debug().Msgf("Creating blueprint scope for user: %s, repo: %s, owner: %s, address: %s, ref: %s",
		user.Username, repoName, ownerName, repoAddress, repoRef)

	scope := &blueprint.BlueprintScope{
		User:          user,
		WorkspaceName: workspaceName,
		Metadata: &models.BlueprintMetadata{
			Name:        blueprintName,
			RepoName:    repoName,
			RepoOwner:   ownerName,
			RepoAddress: repoAddress,
			RepoRef:     repoRef,
		},
	}
	return scope, nil
}

// Start starts the gRPC server and waits for shutdown signals
func (s *Server) Serve() error {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	errChan := make(chan error, 1)
	go func() {
		s.log.Info().Msg("Starting gRPC server")
		if err := s.grpc.Start(); err != nil {
			errChan <- fmt.Errorf("gRPC server error: %v", err)
		}
	}()

	select {
	case sig := <-sigChan:
		s.log.Info().Msgf("Received signal %v, shutting down gracefully", sig)
		s.grpc.Stop()
		s.log.Info().Msg("Server shutdown complete")
		return nil
	case err := <-errChan:
		s.log.Error().Err(err).Msg("Server error occurred")
		return err
	}
}
