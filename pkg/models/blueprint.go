package models

// Blueprint represents a single blueprint configuration
type Blueprint struct {
	Name              string              `yaml:"name" validate:"required,min=1,max=30"`
	Template          string              `yaml:"template"`
	Shell             string              `yaml:"shell" validate:"required"`
	Sudo              bool                `yaml:"sudo" default:"false"`
	Image             string              `yaml:"image" validate:"required"`
	ImagePullSecret   string              `yaml:"imagePullSecret,omitempty"`
	ImagePullPolicy   string              `yaml:"imagePullPolicy,omitempty" validate:"omitempty,oneof=Always Never IfNotPresent"`
	K8shelld          K8shelld            `yaml:"k8shelld" validate:"required"`
	Env               map[string]string   `yaml:"env,omitempty"`
	PortForwarding    []string            `yaml:"portForwarding,omitempty"`
	Network           Network             `yaml:"network" validate:"required"`
	Resources         Resources           `yaml:"resources" validate:"required"`
	Docker            Docker              `yaml:"docker" validate:"required"`
	CredentialHelpers CredentialHelpers   `yaml:"credentialHelpers"`
	Storages          map[string]Storage  `yaml:"storages" validate:"required,min=1,dive"`
	InitScripts       []map[string]string `yaml:"initScripts,omitempty"`
	ServiceAccount    string              `yaml:"serviceAccount,omitempty"`
}

// CustomBlueprint represents a custom blueprint configuration
type CustomBlueprint struct {
	Template       string              `yaml:"template"`
	Shell          string              `yaml:"shell" validate:"required"`
	Sudo           bool                `yaml:"sudo" default:"false"`
	Image          string              `yaml:"image" validate:"required"`
	Env            map[string]string   `yaml:"env,omitempty"`
	PortForwarding []string            `yaml:"portForwarding,omitempty"`
	Network        Network             `yaml:"network" validate:"required"`
	Resources      Resources           `yaml:"resources" validate:"required"`
	Storages       map[string]Storage  `yaml:"storages" validate:"required,min=1,dive"`
	InitScripts    []map[string]string `yaml:"initScripts,omitempty"`
}

// K8shelld represents k8shelld configuration
type K8shelld struct {
	Image           string   `yaml:"image" validate:"required"`
	ImagePullSecret string   `yaml:"imagePullSecret,omitempty"`
	ImagePullPolicy string   `yaml:"imagePullPolicy,omitempty" validate:"omitempty,oneof=Always Never IfNotPresent"`
	EncryptConfig   bool     `yaml:"encryptConfig,omitempty"`
	IgnoreOrphans   []string `yaml:"ignoreOrphans,omitempty"`
	Cert            Cert     `yaml:"cert" validate:"required"`
}

// Cert represents certificate configuration
type Cert struct {
	Country      string `yaml:"country" validate:"required,len=2"`
	State        string `yaml:"state" validate:"required"`
	Locality     string `yaml:"locality" validate:"required"`
	Organization string `yaml:"organization" validate:"required"`
	CommonName   string `yaml:"commonName" validate:"required,fqdn"`
}

// Network represents network configuration
type Network struct {
	NetworkPolicy string   `yaml:"networkPolicy" validate:"required,oneof=workspace system isolated user organization"`
	AllowEgress   []string `yaml:"allowEgress,omitempty" validate:"dive,cidr"`
}

// Resources represents resource limits
type Resources struct {
	CPU    string `yaml:"cpu" validate:"required"`
	Memory string `yaml:"memory" validate:"required"`
}

// Docker represents Docker configuration
type Docker struct {
	Enabled        bool              `yaml:"enabled"`
	Image          string            `yaml:"image" validate:"required_if=Enabled true"`
	Resources      Resources         `yaml:"resources" validate:"required_if=Enabled true"`
	GroupID        int               `yaml:"group_id" validate:"min=0,max=65535"`
	SubGID         int               `yaml:"subgid" validate:"min=0"`
	ParentStorages bool              `yaml:"parentStorages"`
	ExtFiles       map[string]string `yaml:"extFiles,omitempty"`
}

// CredentialHelpers represents credential helper configuration
type CredentialHelpers struct {
	Docker ServerCredentials `yaml:"docker,omitempty"`
	Git    ServerCredentials `yaml:"git,omitempty"`
}

// ServerCredentials represents server credential configuration
type ServerCredentials struct {
	Enabled bool     `yaml:"enabled"`
	Servers []Server `yaml:"servers,omitempty" validate:"dive"`
}

// Server represents a server configuration
type Server struct {
	Address  string `yaml:"address" validate:"required"`
	Username string `yaml:"username" validate:"required"`
	Password string `yaml:"password" validate:"required"`
}

// Storage represents storage configuration
type Storage struct {
	Enabled      bool              `yaml:"enabled"`
	StorageClass string            `yaml:"storageClass" validate:"required_if=Enabled true"`
	Size         string            `yaml:"size" validate:"required_if=Enabled true"`
	Path         string            `yaml:"path" validate:"required_if=Enabled true,startswith=/"`
	Readonly     bool              `yaml:"readonly"`
	Annotations  map[string]string `yaml:"annotations,omitempty"`
}

type Repo struct {
	Name  string `yaml:"name" validate:"required"`
	Owner string `yaml:"owner" validate:"required"`
}

// Validate validates the blueprint and returns user-friendly errors
func (b *Blueprint) Validate() Validator {
	return NewValidator(b)
}
