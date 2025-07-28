package models

// Blueprint represents a single blueprint configuration
type Blueprint struct {
	Name              string              `yaml:"name"`
	Shell             string              `yaml:"shell"`
	Sudo              bool                `yaml:"sudo"`
	Image             string              `yaml:"image"`
	ImagePullSecret   string              `yaml:"imagePullSecret,omitempty"`
	ImagePullPolicy   string              `yaml:"imagePullPolicy,omitempty"`
	K8shelld          K8shelld            `yaml:"k8shelld"`
	Env               map[string]string   `yaml:"env,omitempty"`
	PortForwarding    []string            `yaml:"portForwarding,omitempty"`
	Network           Network             `yaml:"network"`
	Resources         Resources           `yaml:"resources"`
	Docker            Docker              `yaml:"docker"`
	CredentialHelpers CredentialHelpers   `yaml:"credentialHelpers"`
	Storages          map[string]Storage  `yaml:"storages"`
	InitScripts       []map[string]string `yaml:"initScripts,omitempty"`
}

// K8shelld represents k8shelld configuration
type K8shelld struct {
	Image           string   `yaml:"image"`
	ImagePullSecret string   `yaml:"imagePullSecret,omitempty"`
	ImagePullPolicy string   `yaml:"imagePullPolicy,omitempty"`
	EncryptConfig   bool     `yaml:"encryptConfig,omitempty"`
	IgnoreOrphans   []string `yaml:"ignoreOrphans,omitempty"`
	Cert            Cert     `yaml:"cert"`
}

// Cert represents certificate configuration
type Cert struct {
	Country      string `yaml:"country"`
	State        string `yaml:"state"`
	Locality     string `yaml:"locality"`
	Organization string `yaml:"organization"`
	CommonName   string `yaml:"commonName"`
}

// Network represents network configuration
type Network struct {
	NetworkPolicy string   `yaml:"networkPolicy"`
	AllowEgress   []string `yaml:"allowEgress,omitempty"`
}

// Resources represents resource limits
type Resources struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

// Docker represents Docker configuration
type Docker struct {
	Enabled        bool              `yaml:"enabled"`
	Image          string            `yaml:"image"`
	Resources      Resources         `yaml:"resources"`
	GroupID        int               `yaml:"group_id"`
	SubGID         int               `yaml:"subgid"`
	ParentStorages bool              `yaml:"parentStorages"`
	ExtFiles       map[string]string `yaml:"extFiles,omitempty"`
}

// CredentialHelpers represents credential helper configuration
type CredentialHelpers struct {
	Docker ServerCredentials `yaml:"docker,omitempty"`
	Git    ServerCredentials `yaml:"git,omitempty"`
}

// DockerCredentials represents Docker credential configuration
type ServerCredentials struct {
	Enabled bool     `yaml:"enabled"`
	Servers []Server `yaml:"servers,omitempty"`
}

// Server represents a server configuration
type Server struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Storage represents storage configuration
type Storage struct {
	Enabled      bool              `yaml:"enabled"`
	StorageClass string            `yaml:"storageClass"`
	Size         string            `yaml:"size"`
	Path         string            `yaml:"path"`
	Readonly     bool              `yaml:"readonly"`
	Annotations  map[string]string `yaml:"annotations,omitempty"`
}
