package workspace

import (
	"fmt"

	k8shelldcfg "github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/common/pkg/gapi"
)

// buildConfigYAML constructs the k8shelld config.yaml content from the workspace's
// blueprint and server config, returning it as a clean YAML string with no comments
// or Helm templating.
func (w *Workspace) buildConfigYAML() (string, error) {
	namespace := getNamespace()
	bp := w.blueprint

	// Resolve GRPC allowed namespace/SA, respecting blueprint connection overrides.
	nsVal := namespace
	saVal := "ssh-proxy"
	conn := bp.K8shelld.Connection
	if conn.AllowAnyNS {
		nsVal = "*"
	}
	if conn.AllowAnySA {
		saVal = "*"
	}

	grpc := gapi.ServerConfig{
		Port:      2822,
		EnableTLS: w.config.CertManager.Enabled,
		Audience:  "k8shelld",
		Allowed: []gapi.AllowedCaller{
			{Namespace: nsVal, ServiceAccount: saVal},
			{Namespace: namespace, ServiceAccount: "api-server"},
		},
	}
	if w.config.CertManager.Enabled {
		grpc.CertFile = "/etc/tls/k8shelld/tls.crt"
		grpc.KeyFile = "/etc/tls/k8shelld/tls.key"
	}

	cfg := k8shelldcfg.Config{
		System: k8shelldcfg.System{
			PProf: false,
			ApiServer: k8shelldcfg.ApiServer{
				Enabled: w.config.K8shellCapabilities.APIServerEnabled,
				Address: fmt.Sprintf("http://api-server.%s:9101", namespace),
			},
			GrpcConfig: grpc,
		},
		Identity: k8shelldcfg.Identity{
			TokenPath:     "/run/secrets/identity-token/token",
			PublicKeyPath: "/run/secrets/jwt-verifier/public-key.pem",
			SigningMethod: w.config.JWTVerifier.SigningMethod,
		},
		TerminateOrphans: k8shelldcfg.TerminateOrphans{
			Enabled:       true,
			CheckInterval: 5,
			Exclude:       bp.K8shelld.IgnoreOrphans,
		},
		ReapZombies: k8shelldcfg.ReapZombies{Enabled: true},
		Shells:      w.config.K8shellCapabilities.Shells,
	}

	out, err := marshalYAML2(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal k8shelld config YAML: %w", err)
	}
	return string(out), nil
}
