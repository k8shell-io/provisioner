package workspace_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/k8shell-io/provisioner/internal/config"
	"github.com/k8shell-io/provisioner/internal/helm"
	"github.com/k8shell-io/provisioner/internal/workspace"
)

func TestGetWorkspaces(t *testing.T) {
	ctx := context.Background()

	helmClient, err := helm.NewClient("workspaces-staging", config.DefaultRegistry{})
	if err != nil {
		t.Fatalf("failed to create Helm client: %v", err)
	}

	start := time.Now()
	got, err := workspace.GetWorkspaces(ctx, helmClient.KubeClient().CoreV1(),
		"workspaces-staging",
		workspace.GetWorkspacesOptions{
			Username: "vitvatom",
		})
	elapsed := time.Since(start)

	fmt.Printf("GetWorkspaces(%q) took %s\n", "vitvatom", elapsed)

	_ = got
	_ = err

	if err != nil {
		t.Fatalf("GetWorkspaces failed: %v", err)
	}

	for _, ws := range got.Workspaces {
		fmt.Printf("name: %s, host: %s, port: %d, tlsEnabled: %t, appVersion: %s, status: %s, message: %s\n",
			ws.Name, ws.Host, ws.Port, ws.TLSEnabled, ws.AppVersion, ws.PodStatus.Status, ws.PodStatus.Message)
	}
}
