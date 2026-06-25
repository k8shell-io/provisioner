// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// provisioner is the k8shell workspace provisioner daemon. It exposes a gRPC
// API (ProvisionerService) consumed by the k8shell API server to create, stop,
// and delete developer workspaces running as Kubernetes pods or injected into
// existing workloads.
package main

import (
	"flag"
	"fmt"
	"os"

	log "github.com/k8shell-io/common/pkg/logger"
	"github.com/k8shell-io/provisioner/internal/server"
)

var (
	PROVISIONER_VERSION = "0.0.0"
	PROVISIONER_COMMIT  = "0000000"
)

// Options represents the command line options
type Options struct {
	ConfigPath  string
	LogText     bool
	ShowVersion bool
}

// getOptions parses the command line options and returns the Options struct
func getOptions(version string, commit_id string) (*Options, error) {
	// Default options
	options := &Options{
		ConfigPath:  "config/config.yaml",
		LogText:     false,
		ShowVersion: false,
	}

	// Parse command line flags
	flag.StringVar(&options.ConfigPath, "config", options.ConfigPath, "Path to the configuration file")
	flag.BoolVar(&options.LogText, "logtext", options.LogText, "Log in text format (default: JSON)")
	flag.BoolVar(&options.ShowVersion, "v", false, "Show version information")

	// Print usage
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n  provisioner [options]\n")
		fmt.Fprint(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Options:\n")
		fmt.Fprint(os.Stderr, "  --config <file>    Configuration file\n")
		fmt.Fprint(os.Stderr, "  --logtext          Log in text format (default: JSON)\n")
		fmt.Fprint(os.Stderr, "  -v                 Show version and exit\n")
	}

	// Parse the flags
	flag.Parse()
	if options.ShowVersion {
		fmt.Printf("provisioner version: %s (commit: %s)\n", version, commit_id)
		os.Exit(0)
	}

	return options, nil
}

func main() {
	opts, err := getOptions(PROVISIONER_VERSION, PROVISIONER_COMMIT)
	if err != nil {
		fmt.Printf("Error parsing options: %v\n", err)
		os.Exit(1)
	}

	log.JsonLogger = !opts.LogText
	log := log.NewLogger("server")

	server, err := server.NewServer(opts.ConfigPath, PROVISIONER_VERSION, PROVISIONER_COMMIT)
	if err != nil {
		log.Error().Msgf("Error starting server: %v", err)
		return
	}

	err = server.Serve()
	if err != nil {
		log.Error().Msgf("Error running server: %v", err)
	}
}
