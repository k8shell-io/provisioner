package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/k8shell-io/common/pkg/api/client/identity"
	provisionerv1 "github.com/k8shell-io/common/pkg/api/gen/go/provisioner/v1"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/gapi"
	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/common/pkg/models"
	natsc "github.com/k8shell-io/common/pkg/nats"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

type Server struct {
	config          *config.Config
	log             *zerolog.Logger
	nats            *natsc.NATSClient
	Identity        *identity.IdentityClient
	tokenVerifier   *authz.JWTVerifier
	grpc            *gapi.Server
	bpManager       *blueprint.BlueprintManager
	helm            *helm.Client
	provisionJobsKV *natsc.JetStreamKV
}

func NewServer(configFile string, appVersion string, commit string) (*Server, error) {
	server := &Server{
		log: log.NewLogger("server"),
	}

	server.log.Info().Str("version", appVersion).Str("commit", commit).Msg("Starting provisioner")

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
		Strategies: blueprint.MergeStrategies{
			// claimSpec.accessModes: child replaces parent entirely — appending access modes makes no sense.
			// Using the full path avoids colliding with any other "accessModes" key elsewhere in the tree.
			"claimSpec.accessModes": func(_, child []interface{}) []interface{} {
				return child
			},
			// initScripts: child entries with a matching name replace the parent entry;
			// entries with unique names are appended.
			"initScripts": func(parent, child []interface{}) []interface{} {
				result := make([]interface{}, 0, len(parent))
				childByName := make(map[string]interface{})
				for _, item := range child {
					if m, ok := item.(map[string]interface{}); ok {
						if name, ok := m["name"].(string); ok && name != "" {
							childByName[name] = item
						}
					}
				}
				for _, item := range parent {
					if m, ok := item.(map[string]interface{}); ok {
						if name, ok := m["name"].(string); ok {
							if override, exists := childByName[name]; exists {
								result = append(result, override)
								delete(childByName, name)
								continue
							}
						}
					}
					result = append(result, item)
				}
				// append any child scripts that didn't exist in parent
				for _, item := range child {
					if m, ok := item.(map[string]interface{}); ok {
						if name, ok := m["name"].(string); ok && name != "" {
							if _, remaining := childByName[name]; remaining {
								result = append(result, item)
							}
						}
					}
				}
				return result
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create blueprint manager: %w", err)
	}

	if server.config.Nats.Enabled {
		server.nats, err = natsc.NewNATSClient(server.config.Nats.NATSClientConfig)
		if err != nil {
			return nil, fmt.Errorf("create nats client: %w", err)
		}
		if server.nats == nil {
			return nil, fmt.Errorf("nats is required for api-server to store sessions")
		}

		server.provisionJobsKV, err = server.nats.NewKV(natsc.BucketOptions{
			Bucket:    natsc.WORKSPACE_PROVISION_JOBS_BUCKET,
			BucketTTL: server.config.Nats.KV.ProvisionBucketTTL,
		})
		if err != nil {
			return nil, fmt.Errorf("create provision jobs kv: %w", err)
		}
	}

	server.Identity, err = identity.NewIdentityClient(server.config.Identity)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity client: %w", err)
	}

	server.tokenVerifier, err = authz.NewJWTVerifier(server.config.JWTVerifier)
	if err != nil {
		return nil, fmt.Errorf("failed to create identity token verifier: %w", err)
	}

	server.log.Info().Msg("Creating Helm client")
	server.helm, err = helm.NewClient(server.config.TargetNamespace, server.config.DefaultRegistry)
	if err != nil {
		return nil, fmt.Errorf("failed to create Helm client: %w", err)
	}

	pk, err := server.config.JWTVerifier.GetPublicKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get public key for JWT verifier: %w", err)
	}
	server.helm.JWTVerifierPublicKey = pk
	server.helm.AppVersion = appVersion
	server.helm.Commit = commit

	server.log.Info().Msg("Creating gRPC service")
	server.grpc, err = gapi.NewServer(&server.config.GrpcConfig, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC service: %w", err)
	}

	err = server.grpc.RegisterService(func(s *grpc.Server) error {
		provisionerv1.RegisterProvisionerServiceServer(s, NewProvisionerService(server))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register gRPC service: %w", err)
	}

	server.log.Info().Msgf("Ensuring workspace base, namespace %s", server.config.TargetNamespace)
	if err := server.helm.EnsureBase(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ensure base namespace: %w", err)
	}

	return server, nil
}

func (s *Server) GetBlueprintScope(blueprintName string, user *models.User,
	bpMetadata *models.BlueprintMetadata, workspaceName string) (*blueprint.BlueprintScope, error) {

	if blueprintName == "" {
		return nil, fmt.Errorf("blueprint name is required to create scope")
	}

	var repoName = "noreponame"
	var ownerName = "norepoowner"
	var repoAddress = "noaddress"
	var repoRef = ""

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
			Name:        blueprint.NormalizeDNSLabel(blueprintName),
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start injection watcher when specific namespaces are configured (not cluster-wide).
	if len(s.config.InjectNamespaces) > 0 && !s.config.IsClusterWideInjectionEnabled() {
		watcher := NewInjectionWatcher(s.helm, s.config.InjectNamespaces)
		go watcher.Run(ctx)
	}

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
		cancel()
		s.grpc.Stop()
		s.log.Info().Msg("Server shutdown complete")
		return nil
	case err := <-errChan:
		s.log.Error().Err(err).Msg("Server error occurred")
		return err
	}
}
