package workspace

import (
	"fmt"

	k8shelldcfg "github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/common/pkg/gapi"
	"gopkg.in/yaml.v3"
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

	// Only include enabled apps in the config file.
	var apps k8shelldcfg.Apps
	for name, app := range bp.Apps {
		if !app.Enabled {
			continue
		}
		if apps == nil {
			apps = make(k8shelldcfg.Apps)
		}
		apps[name] = &k8shelldcfg.AppSpec{
			Binary:        app.Binary,
			VersionCmd:    app.VersionCmd,
			VersionRegex:  app.VersionRegex,
			Install:       app.Install,
			Start:         app.Start,
			Listen:        app.Listen,
			RestartPolicy: app.RestartPolicy,
			InstallAsRoot: app.InstallAsRoot,
			AutoStart:     app.AutoStart,
			Protocol:      app.Protocol,
		}
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
			SigningMethod:  w.config.JWTVerifier.SigningMethod,
		},
		Splash: bp.Splash,
		TerminateOrphans: k8shelldcfg.TerminateOrphans{
			Enabled:       true,
			CheckInterval: 5,
			Exclude:       bp.K8shelld.IgnoreOrphans,
		},
		ReapZombies: k8shelldcfg.ReapZombies{Enabled: true},
		Podman: k8shelldcfg.PodmanConfig{
			Enabled:                 bp.Podman.Enabled,
			CreateDockerSockSymlink: bp.Podman.CreateDockerSockSymlink,
		},
		EnableApps: bp.EnableApps,
		Apps:       apps,
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal k8shelld config YAML: %w", err)
	}
	return string(out), nil
}
