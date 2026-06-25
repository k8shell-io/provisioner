// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package helm

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

//go:embed charts/k8shell-workspace/*
//go:embed charts/k8shell-workspace/templates/*
//go:embed charts/k8shell-workspace/templates/_helpers.tpl
var chartFS embed.FS

// GetChartFS returns the embedded filesystem containing Helm charts
func GetChartFS() fs.FS {
	subFS, err := fs.Sub(chartFS, "charts")
	if err != nil {
		panic(err)
	}
	return subFS
}

// LoadChartFromMemory loads a Helm chart from embedded filesystem
func LoadChartFromMemory(chartName string) (*chart.Chart, error) {
	chartFS := GetChartFS()

	tempDir, err := os.MkdirTemp("", "helm-chart-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	chartPath := filepath.Join(tempDir, chartName)
	if err := fs.WalkDir(chartFS, chartName, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		destPath := filepath.Join(tempDir, path)

		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}

		// Read file from embedded FS
		data, err := fs.ReadFile(chartFS, path)
		if err != nil {
			return err
		}

		// Write to temp directory
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		return os.WriteFile(destPath, data, 0644)
	}); err != nil {
		return nil, fmt.Errorf("failed to extract chart: %w", err)
	}

	chart, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load chart: %w", err)
	}

	return chart, nil
}
