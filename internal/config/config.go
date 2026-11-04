// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

// Package config defines the server configuration structures and the loader
// that parses, validates, and post-processes the YAML configuration file
// supplied to the provisioner on startup.
package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/common/pkg/authz"
	"github.com/k8shell-io/common/pkg/config"
	"github.com/k8shell-io/common/pkg/db"
	"github.com/k8shell-io/common/pkg/gapi"
	natsc "github.com/k8shell-io/common/pkg/nats"
	"github.com/k8shell-io/common/pkg/validator"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Config represents the server configuration
type Config struct {
	TargetNamespace  string   `yaml:"targetNamespace" validate:"required"`
	InjectNamespaces []string `yaml:"injectNamespaces"`
	// NodeSelectorKeys are the node label keys (e.g. "k8shell.io/pool") whose
	// values are discovered from cluster nodes and offered as placement
	// options. Discovery needs cluster-wide get/list/watch on nodes.
	NodeSelectorKeys    []string                `yaml:"nodeSelectorKeys"`
	ClusterDomain       string                  `yaml:"clusterDomain"`
	DefaultRegistry     DefaultRegistry         `yaml:"defaultRegistry"`
	PrivateRegistry     PrivateRegistry         `yaml:"privateRegistry"`
	K8shellCapabilities K8shellCapabilities     `yaml:"k8shellCapabilities"`
	CertManager         CertManagerConfig       `yaml:"certManager"`
	GrpcConfig          gapi.ServerConfig       `yaml:"grpc" validate:"required"`
	Nats                ProvisionerNatsConfig   `yaml:"nats"`
	Identity            gapi.ClientConfig       `yaml:"identity"`
	Authz               gapi.ClientConfig       `yaml:"authz"`
	JWTVerifier         authz.JWTVerifierConfig `yaml:"jwtVerifier" validate:"required"`
	Blueprints          BlueprintsFileConfig    `yaml:"blueprints"`
	BaseDir             string                  `yaml:"baseDir"`
	KubeClient          KubeClientConfig        `yaml:"kubeClient"`
	Schedule            ScheduleConfig          `yaml:"schedule"`

	// DB configures the database connection used to persist org-scoped
	// blueprint definitions, layered on top of the file-based blueprints
	// loaded from Blueprints.Directory. Disabled (the default) means org
	// blueprints are unavailable and only file-based blueprints are served.
	DB db.DBConfig `yaml:"db"`
}

// Schedule enforcement modes. See ScheduleConfig.Enforcement.
const (
	ScheduleEnforcementOff     = "off"
	ScheduleEnforcementDryRun  = "dry-run"
	ScheduleEnforcementEnforce = "enforce"
)

// DefaultScheduleInterval is how often workspace schedules are evaluated when
// ScheduleConfig.Interval is not set.
const DefaultScheduleInterval = 30 * time.Second

// ScheduleConfig configures the enforcement of blueprint schedules (see
// models.Schedule): warning users, stopping workspaces whose schedule has
// ended and terminating them after their terminate grace period.
type ScheduleConfig struct {
	// Enforcement is "off" (the default: schedules are shown but nothing acts
	// on them), "dry-run" (log what would be done, change nothing) or
	// "enforce".
	Enforcement string `yaml:"enforcement"`
	// IntervalRaw is how often schedules are evaluated, as a duration such as
	// "30s". Defaults to DefaultScheduleInterval.
	IntervalRaw string        `yaml:"interval"`
	Interval    time.Duration `yaml:"-"`
}

// normalize validates c and fills in its defaults and derived fields.
func (c *ScheduleConfig) normalize() error {
	switch c.Enforcement {
	case "":
		c.Enforcement = ScheduleEnforcementOff
	case ScheduleEnforcementOff, ScheduleEnforcementDryRun, ScheduleEnforcementEnforce:
	default:
		return fmt.Errorf("invalid schedule.enforcement %q: must be %s, %s or %s", c.Enforcement,
			ScheduleEnforcementOff, ScheduleEnforcementDryRun, ScheduleEnforcementEnforce)
	}
	c.Interval = DefaultScheduleInterval
	if c.IntervalRaw != "" {
		d, err := time.ParseDuration(c.IntervalRaw)
		if err != nil || d < time.Second {
			return fmt.Errorf("invalid schedule.interval %q: must be a duration of at least 1s", c.IntervalRaw)
		}
		c.Interval = d
	}
	return nil
}

// ProvisionerNatsConfig represents the NATS configuration for the provisioner
type ProvisionerNatsConfig struct {
	natsc.NATSClientConfig `yaml:",inline" mapstructure:",squash"`
	KV                     JobsKVConfig `yaml:"kv" mapstructure:"kv"`
}

// JobsKVConfig represents the configuration for the NATS Key-Value store used for provisioning jobs
type JobsKVConfig struct {
	ProvisionBucketTTL    time.Duration `yaml:"-"`
	ProvisionBucketTTLRaw string        `yaml:"provisionBucketTTL"`
}

// K8shellCapabilities represents the capabilities of the k8shell environment
type K8shellCapabilities struct {
	APIServerEnabled bool             `yaml:"apiServerEnabled"`
	SaToken          k8shelld.SaToken `yaml:"saToken,omitempty"`
	Shells           k8shelld.Shells  `yaml:"shells,omitempty"`
}

// CertManagerConfig represents the cert-manager configuration
type CertManagerConfig struct {
	Enabled     bool       `yaml:"enabled"`
	Issuer      CertIssuer `yaml:"issuer" validate:"required_if=Enabled true"`
	Duration    string     `yaml:"duration"`
	RenewBefore string     `yaml:"renewBefore"`
}

// CertIssuer represents the certificate issuer configuration
type CertIssuer struct {
	Name string `yaml:"name" validate:"required"`
	Kind string `yaml:"kind" validate:"required,oneof=ClusterIssuer Issuer"`
}

// DefaultRegistry is the default container registry host used to prefix image names.
type DefaultRegistry struct {
	Host string `yaml:"host"`
}

// PrivateRegistry holds credentials for a private registry used to create imagePullSecrets
// and a registry CA configmap. It is optional — if not set, no regcred is created.
type PrivateRegistry struct {
	Host     string `yaml:"host"`
	CertCA   string `yaml:"certCA"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Default cluster domain is "cluster.local", but it can be overridden by configuration.
// This is needed for constructing the FQDN of workspace pods
var ClusterDomain string = "cluster.local"

// ToValues returns the registry fields as a Helm values map.
func (r DefaultRegistry) ToValues() map[string]interface{} {
	return map[string]interface{}{
		"host": r.Host,
	}
}

// ToValues returns the private registry fields as a Helm values map, including
// the base64-encoded dockerConfigJson when credentials are configured.
func (r PrivateRegistry) ToValues() map[string]interface{} {
	values := map[string]interface{}{
		"certCA": r.CertCA,
	}
	if r.Username != "" && r.Password != "" {
		values["dockerConfigJson"] = fmt.Sprintf(`{"auths": {"%s": {"username": "%s","password": "%s"}}}`,
			r.Host, r.Username, r.Password)
		values["regcred"] = "regcred"
	}
	return values
}

// Blueprint represents a blueprint configuration
type BlueprintsFileConfig struct {
	Directory              string `yaml:"directory" validate:"required"`
	DefaultCustomBlueprint string `yaml:"defaultCustomBlueprint"`
}

// KubeClientConfig tunes the rate limiting of the in-cluster Kubernetes
// client used for all workspace pod/release lookups and mutations. client-go
// defaults to 5 QPS / 10 burst when unset, which throttles the provisioner
// client-side under concurrent request load; DefaultKubeClientQPS and
// DefaultKubeClientBurst below are applied when the config omits either
// field.
type KubeClientConfig struct {
	QPS   float32 `yaml:"qps"`
	Burst int     `yaml:"burst"`
}

// Default Kubernetes client-side rate limits, applied when kubeClient.qps or
// kubeClient.burst is left unset (zero) in the config file. These are
// hardcoded starting points, not tuned for any particular deployment size —
// raised from 50/100 after tracing ~24-46ms client-side throttling waits
// (see rest.Config.QPS/Burst) on Helm release-list cache misses under
// bursty request load in workspaces-staging.
const (
	DefaultKubeClientQPS   float32 = 150
	DefaultKubeClientBurst int     = 300
)

// NewConfig loads and fully validates the server configuration from configFile.
// Post-load steps include: parsing duration strings, applying defaults for
// cert-manager, validating the JWT signing method, deduplicating injection
// namespaces, and resolving relative paths against the config file directory.
func NewConfig(configFile string) (*Config, error) {
	var cfg Config

	processor := config.NewDefaultProcessor()
	if err := processor.LoadAndDecode(configFile, &cfg); err != nil {
		return nil, fmt.Errorf("failed to load configuration from '%s': %w", configFile, err)
	}

	// Structural validation via common validator
	if errs := validator.NewValidator(cfg); !errs.IsValid() {
		return nil, fmt.Errorf("invalid configuration:\n%s", errs.ErrorMessages())
	}

	// Post-load derived fields and cross-field checks
	if cfg.Nats.KV.ProvisionBucketTTLRaw != "" {
		d, err := time.ParseDuration(cfg.Nats.KV.ProvisionBucketTTLRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid nats.kv.provisionBucketTTL %q: %w", cfg.Nats.KV.ProvisionBucketTTLRaw, err)
		}
		cfg.Nats.KV.ProvisionBucketTTL = d
	}

	if err := cfg.Schedule.normalize(); err != nil {
		return nil, err
	}

	if cfg.CertManager.Enabled {
		if cfg.CertManager.Duration == "" {
			cfg.CertManager.Duration = "24h"
		}
		if cfg.CertManager.RenewBefore == "" {
			cfg.CertManager.RenewBefore = "12h"
		}
	}

	method := cfg.JWTVerifier.SigningMethod
	if method != "es256" && method != "rs256" {
		return nil, fmt.Errorf("jwtVerifier.signingMethod %q is not supported; must be es256 or rs256", method)
	}
	if cfg.JWTVerifier.PublicKeyFile == "" && cfg.JWTVerifier.PrivateKeyFile == "" {
		return nil, fmt.Errorf("jwtVerifier: publicKeyFile or privateKeyFile is required")
	}

	if cfg.ClusterDomain == "" {
		cfg.ClusterDomain = ClusterDomain
	}
	ClusterDomain = cfg.ClusterDomain

	if cfg.KubeClient.QPS == 0 {
		cfg.KubeClient.QPS = DefaultKubeClientQPS
	}
	if cfg.KubeClient.Burst == 0 {
		cfg.KubeClient.Burst = DefaultKubeClientBurst
	}

	for i := range cfg.InjectNamespaces {
		ns := strings.TrimSpace(cfg.InjectNamespaces[i])
		if ns == "" {
			return nil, fmt.Errorf("injectNamespaces[%d] must not be empty", i)
		}
		cfg.InjectNamespaces[i] = ns
	}

	keys, err := normalizeNodeSelectorKeys(cfg.NodeSelectorKeys)
	if err != nil {
		return nil, err
	}
	cfg.NodeSelectorKeys = keys

	cfg.BaseDir = filepath.Dir(configFile)

	injectedNamespaces := make([]string, 0, len(cfg.InjectNamespaces))
	seenNamespaces := make(map[string]struct{}, len(cfg.InjectNamespaces))
	for _, ns := range cfg.InjectNamespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if ns == "*" {
			injectedNamespaces = []string{"*"}
			break
		}
		if _, exists := seenNamespaces[ns]; exists {
			continue
		}
		seenNamespaces[ns] = struct{}{}
		injectedNamespaces = append(injectedNamespaces, ns)
	}
	cfg.InjectNamespaces = injectedNamespaces

	return &cfg, nil
}

// AllowsInjectionNamespace returns true when the namespace is permitted by
// injectNamespaces. A literal "*" allows all namespaces.
// Returns false when injectNamespaces is empty, effectively disabling injection.
func (c *Config) AllowsInjectionNamespace(namespace string) bool {
	if c == nil || namespace == "" || len(c.InjectNamespaces) == 0 {
		return false
	}
	for _, ns := range c.InjectNamespaces {
		if ns == "*" || ns == namespace {
			return true
		}
	}
	return false
}

// IsClusterWideInjectionEnabled returns true if injectNamespaces includes "*".
func (c *Config) IsClusterWideInjectionEnabled() bool {
	if c == nil {
		return false
	}
	for _, ns := range c.InjectNamespaces {
		if ns == "*" {
			return true
		}
	}
	return false
}

// normalizeNodeSelectorKeys trims the keys, drops duplicates and rejects any
// that is not a valid Kubernetes label key.
func normalizeNodeSelectorKeys(in []string) ([]string, error) {
	var out []string
	seen := make(map[string]struct{}, len(in))
	for i, k := range in {
		k = strings.TrimSpace(k)
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return nil, fmt.Errorf("nodeSelectorKeys[%d] %q is not a valid label key: %s", i, k, strings.Join(errs, "; "))
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out, nil
}
