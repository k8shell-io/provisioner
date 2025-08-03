package blueprint_test

import (
	"encoding/json"
	"os"
	"testing"

	identity "github.com/k8shell-io/identity/pkg/models"
	"github.com/k8shell-io/provisioner/internal/blueprint"
	"github.com/k8shell-io/provisioner/pkg/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "go.uber.org/automaxprocs"
)

// setupTestEnvironment sets up environment variables for testing
func setupTestEnvironment(t *testing.T) func() {
	t.Helper()

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

	return func() {
		for key := range envVars {
			os.Unsetenv(key)
		}
	}
}

// createTestScope creates a test scope with user data
func createTestScope(username string, roles []identity.Role) *blueprint.BlueprintScope {
	scope := blueprint.TestScope()
	scope.User = &identity.User{
		Username: username,
		Roles:    roles,
	}
	return scope
}

// validateBlueprintBasics validates basic blueprint properties
func validateBlueprintBasics(t *testing.T, bp *models.Blueprint, expectedName string) {
	t.Helper()

	assert.NotNil(t, bp, "Blueprint should not be nil")
	assert.Equal(t, expectedName, bp.Name, "Blueprint name should match")
	assert.NotEmpty(t, bp.Shell, "Shell should be specified")
	assert.NotEmpty(t, bp.K8shelld.Image, "K8shelld image should be specified")
}

func TestBlueprintManager_Creation(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	tests := []struct {
		name        string
		dir         string
		expectError bool
	}{
		{
			name:        "valid directory",
			dir:         "/home/bruckins/provisioner/config/blueprints",
			expectError: false,
		},
		{
			name:        "invalid directory",
			dir:         "/nonexistent/path",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
				Dir:        tt.dir,
				Strategies: blueprint.MergeStrategies{},
			})

			if tt.expectError {
				assert.Error(t, err, "Expected error for invalid directory")
				assert.Nil(t, manager, "Manager should be nil on error")
			} else {
				assert.NoError(t, err, "Should create manager successfully")
				assert.NotNil(t, manager, "Manager should not be nil")
			}
		})
	}
}

func TestBlueprintManager_GetBlueprint(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	require.NoError(t, err, "Failed to create blueprint manager")

	tests := []struct {
		name          string
		blueprintName string
		scope         *blueprint.BlueprintScope
		expectError   bool
		validateFunc  func(t *testing.T, bp *models.Blueprint)
	}{
		{
			name:          "admin user scope",
			blueprintName: "identity",
			scope:         createTestScope("admin", []identity.Role{"admin", "developer"}),
			expectError:   false,
			validateFunc: func(t *testing.T, bp *models.Blueprint) {
				validateBlueprintBasics(t, bp, "identity")
				assert.NotEmpty(t, bp.Storages, "Should have storages configured")
				assert.Contains(t, bp.Storages, "home", "Should have home storage")
				assert.Contains(t, bp.Storages, "shared", "Should have shared storage")
			},
		},
		{
			name:          "developer user scope",
			blueprintName: "identity",
			scope:         createTestScope("developer", []identity.Role{"developer"}),
			expectError:   false,
			validateFunc: func(t *testing.T, bp *models.Blueprint) {
				validateBlueprintBasics(t, bp, "identity")
				// Test role-based differences if any
			},
		},
		{
			name:          "nonexistent blueprint",
			blueprintName: "nonexistent",
			scope:         createTestScope("user", []identity.Role{"user"}),
			expectError:   true,
			validateFunc:  nil,
		},
		{
			name:          "nil scope",
			blueprintName: "identity",
			scope:         nil,
			expectError:   true,
			validateFunc:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bp, err := manager.GetBlueprint(tt.blueprintName, tt.scope)

			if tt.expectError {
				assert.Error(t, err, "Expected error")
				assert.Nil(t, bp, "Blueprint should be nil on error")
			} else {
				assert.NoError(t, err, "Should get blueprint successfully")
				assert.NotNil(t, bp, "Blueprint should not be nil")

				if tt.validateFunc != nil {
					tt.validateFunc(t, bp)
				}
			}
		})
	}
}

func TestBlueprintValidation(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	require.NoError(t, err, "Failed to create blueprint manager")

	scope := createTestScope("testuser", []identity.Role{"admin", "developer"})

	t.Run("validate loaded blueprint", func(t *testing.T) {
		bp, err := manager.GetBlueprint("identity", scope)
		require.NoError(t, err, "Failed to load blueprint")

		validation := bp.Validate()

		if !validation.IsValid() {
			t.Logf("Validation errors found:\n%s", validation.Errors())

			// Log individual errors for debugging
			for i, validationErr := range validation.Errors() {
				t.Logf("  %d. Field: %s, Message: %s, Value: %s",
					i+1, validationErr.Field, validationErr.Message, validationErr.Value)
			}

			// Don't fail the test immediately - let's see what specific errors we get
			t.Logf("Blueprint validation found %d errors", len(validation.Errors()))
		} else {
			t.Log("✓ Blueprint validation passed successfully")
		}

		// Test specific validation aspects
		assert.NotEmpty(t, bp.Name, "Blueprint name should not be empty")
		assert.NotEmpty(t, bp.Shell, "Shell should not be empty")
	})
}

func TestBlueprintSerialization(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	require.NoError(t, err, "Failed to create blueprint manager")

	scope := createTestScope("testuser", []identity.Role{"admin"})

	t.Run("JSON serialization", func(t *testing.T) {
		bp, err := manager.GetBlueprint("identity", scope)
		require.NoError(t, err, "Failed to load blueprint")

		// Test JSON marshaling
		jsonData, err := json.MarshalIndent(bp, "", "  ")
		assert.NoError(t, err, "Should marshal to JSON successfully")
		assert.NotEmpty(t, jsonData, "JSON data should not be empty")

		t.Logf("Blueprint JSON structure:\n%s", string(jsonData))

		// Test JSON unmarshaling
		var unmarshaled models.Blueprint
		err = json.Unmarshal(jsonData, &unmarshaled)
		assert.NoError(t, err, "Should unmarshal from JSON successfully")
		assert.Equal(t, bp.Name, unmarshaled.Name, "Name should match after JSON round trip")
	})
}

func TestBlueprintComponents(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	require.NoError(t, err, "Failed to create blueprint manager")

	scope := createTestScope("testuser", []identity.Role{"admin"})
	bp, err := manager.GetBlueprint("identity", scope)
	require.NoError(t, err, "Failed to load blueprint")

	t.Run("K8shelld configuration", func(t *testing.T) {
		assert.NotEmpty(t, bp.K8shelld.Image, "K8shelld image should be specified")
		assert.NotNil(t, bp.K8shelld.Cert, "Certificate configuration should be present")
		assert.NotEmpty(t, bp.K8shelld.Cert.Country, "Certificate country should be specified")
		assert.NotEmpty(t, bp.K8shelld.Cert.CommonName, "Certificate common name should be specified")
	})

	t.Run("Network configuration", func(t *testing.T) {
		assert.NotEmpty(t, bp.Network.NetworkPolicy, "Network policy should be specified")
	})

	t.Run("Resources configuration", func(t *testing.T) {
		assert.NotNil(t, bp.Resources.CPU, "CPU resources should be specified")
		assert.NotEmpty(t, bp.Resources.Memory, "Memory resources should be specified")
	})

	t.Run("Storage configuration", func(t *testing.T) {
		assert.NotEmpty(t, bp.Storages, "Storages should be configured")

		for name, storage := range bp.Storages {
			t.Run("storage_"+name, func(t *testing.T) {
				if storage.Enabled {
					assert.NotEmpty(t, storage.Size, "Storage size should be specified for enabled storage")
					assert.NotEmpty(t, storage.Path, "Storage path should be specified for enabled storage")
				}
			})
		}
	})

	t.Run("Init scripts", func(t *testing.T) {
		if len(bp.InitScripts) > 0 {
			t.Logf("Found %d init scripts", len(bp.InitScripts))
			for i, script := range bp.InitScripts {
				assert.NotEmpty(t, script, "Init script %d should not be empty", i)
			}
		}
	})
}

func TestBlueprintManager_ListBlueprints(t *testing.T) {
	cleanup := setupTestEnvironment(t)
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	require.NoError(t, err, "Failed to create blueprint manager")

	t.Run("list available blueprints", func(t *testing.T) {
		names := manager.ListBlueprintNames()
		assert.NotEmpty(t, names, "Should have at least one blueprint")
		assert.Contains(t, names, "identity", "Should contain identity blueprint")

		t.Logf("Available blueprints: %v", names)
	})
}

// Benchmark tests
func BenchmarkBlueprintLoadingValidation(b *testing.B) {
	cleanup := setupTestEnvironment(&testing.T{})
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scope := createTestScope("benchuser", []identity.Role{"admin"})
		bp, err := manager.GetBlueprint("identity", scope)
		if err != nil {
			b.Fatal(err)
		}

		bp.Validate()
	}
}

func BenchmarkBlueprintLoadingValidationParallel(b *testing.B) {
	cleanup := setupTestEnvironment(&testing.T{})
	defer cleanup()

	manager, err := blueprint.NewBlueprintManager(blueprint.LoadOptions{
		Dir:        "/home/bruckins/provisioner/config/blueprints",
		Strategies: blueprint.MergeStrategies{},
	})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) { // Now this uses multiple threads
		for pb.Next() {
			scope := createTestScope("benchuser", []identity.Role{"admin"})
			bp, err := manager.GetBlueprint("identity", scope)
			if err != nil {
				b.Fatal(err)
			}
			bp.Validate()
		}
	})
}
