package blueprint_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/stretchr/testify/require"
)

func TestLoadAndResolveBlueprints(t *testing.T) {
	dir := "/home/bruckins/provisioner/config/blueprints"

	// Set environment variables for testing
	envVars := map[string]string{
		"WORKSPACE_REGCRED":           "regcred",
		"WORKSPACE_REGISTRY":          "registry.example.com",
		"WORKSPACE_REGISTRY_USERNAME": "user",
		"WORKSPACE_REGISTRY_PASSWORD": "password",
		"WORKSPACE_STORAGE_CLASS":     "standard",
	}
	for key, value := range envVars {
		err := os.Setenv(key, value)
		require.NoError(t, err, "Failed to set environment variable %s", key)
	}
	defer func() {
		for key := range envVars {
			err := os.Unsetenv(key)
			require.NoError(t, err, "Failed to unset environment variable %s", key)
		}
	}()

	// Basic usage
	scope := map[string]any{
		"username": "bruckins",
		"uid":      1001,
		"user": map[string]any{
			"name":  "John Doe",
			"email": "",
			"roles": []string{"admin", "developer"},
		},
		"repo": map[string]any{
			"owner": "k8shell-io",
			"name":  "provisioner",
		},
	}

	// Advanced usage with custom strategies
	blueprints, err := blueprint.LoadBlueprints(blueprint.LoadOptions{
		Dir:   dir,
		Scope: scope,
	})
	require.NoError(t, err, "Failed to load blueprints")

	// blueprints, err := blueprint.LoadBlueprints(dir)
	// require.NoError(t, err)

	out, err := json.MarshalIndent(blueprints["development"].Raw, "", "  ")
	require.NoError(t, err)
	fmt.Println(string(out))
}
