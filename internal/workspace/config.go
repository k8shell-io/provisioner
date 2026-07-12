// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package workspace

import (
	"encoding/json"
	"fmt"

	k8shelldcfg "github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/common/pkg/models"
)

// buildConfigYAML constructs the k8shelld config.yaml content from the workspace's
// blueprint and server config, returning it as a clean YAML string with no comments
// or Helm templating.
func (w *Workspace) buildConfigYAML(customNamespace string) (string, error) {
	namespace := getNamespace()
	if customNamespace != "" {
		namespace = customNamespace
	}
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
		TerminateOrphans: k8shelldcfg.TerminateOrphans{
			Enabled:       true,
			CheckInterval: 5,
			Exclude:       bp.K8shelld.IgnoreOrphans,
		},
		ReapZombies: k8shelldcfg.ReapZombies{Enabled: true},
		SaToken:     w.config.K8shellCapabilities.SaToken,
		Shells:      w.config.K8shellCapabilities.Shells,
	}

	out, err := marshalYAML2(cfg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal k8shelld config YAML: %w", err)
	}
	return string(out), nil
}

// buildProfileYAML constructs the workspace's /etc/k8shell/profile.yaml content
// from the provisioning user, using models.UserProfile as the canonical shape.
// It round-trips through JSON so the YAML keys match UserProfile's json tags.
func (w *Workspace) buildProfileYAML() (string, error) {
	u := w.user
	profile := models.UserProfile{
		Username:      u.Username,
		Organization:  u.Organization,
		Fullname:      u.Fullname,
		Email:         u.Email,
		UID:           u.UID,
		GID:           u.GID,
		Shell:         u.Shell,
		Sudo:          u.Sudo,
		Source:        u.Source,
		Roles:         u.Roles,
		Blueprints:    u.Blueprints,
		AccountLocked: u.Locked,
	}

	jsonBytes, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("failed to marshal user profile to JSON: %w", err)
	}
	var profileMap map[string]interface{}
	if err := json.Unmarshal(jsonBytes, &profileMap); err != nil {
		return "", fmt.Errorf("failed to unmarshal user profile JSON: %w", err)
	}

	out, err := marshalYAML2(profileMap)
	if err != nil {
		return "", fmt.Errorf("failed to marshal user profile YAML: %w", err)
	}
	return string(out), nil
}
