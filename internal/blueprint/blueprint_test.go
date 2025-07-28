package blueprint_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/k8shell-io/provisioner/internal/blueprint"
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

	blueprints, err := blueprint.LoadBlueprints(dir)
	require.NoError(t, err)

	out, err := json.MarshalIndent(blueprints["development"].Raw, "", "  ")
	require.NoError(t, err)
	fmt.Println(string(out))
}
