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
		os.Setenv(key, value)
	}
	defer func() {
		for key := range envVars {
			os.Unsetenv(key)
		}
	}()

	// Basic usage
	scope1 := map[string]any{
		"username": "bruckins",
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

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        dir,
		Strategies: blueprint.MergeStrategies{},
	})
	require.NoError(t, err, "Failed to create blueprint manager")

	blueprint, err := manager.GetBlueprint("identity", scope1)
	require.NoError(t, err, "Failed to load blueprints")

	v := blueprint.Validate()
	if v != nil {
		t.Fatalf("Validation failed: %v", v.Error())
	}

	out, err := json.MarshalIndent(blueprint, "", "  ")
	require.NoError(t, err)
	fmt.Println(string(out))
	fmt.Println("---")

	// scope2 := map[string]any{
	// 	"username": "alice",
	// 	"user": map[string]any{
	// 		"name":  "John Doe",
	// 		"email": "",
	// 		"roles": []string{"developer"},
	// 	},
	// 	"repo": map[string]any{
	// 		"owner": "k8shell-io",
	// 		"name":  "identity",
	// 	},
	// }

	// blueprint, err = manager.GetBlueprint("identity", scope2)
	// //require.NoError(t, err, "Failed to load blueprints")

	// out, err = json.MarshalIndent(blueprint.Raw, "", "  ")
	// require.NoError(t, err)
	// fmt.Println(string(out))

}
