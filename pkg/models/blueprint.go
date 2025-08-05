package models

import (
	"bytes"
	"fmt"

	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v3"
)

// Blueprint represents a single blueprint configuration
type Blueprint struct {
	Name            string              `yaml:"name" validate:"required,min=1,max=30"`
	Template        string              `yaml:"template"`
	Shell           string              `yaml:"shell" validate:"required"`
	Sudo            bool                `yaml:"sudo" default:"false"`
	Image           string              `yaml:"image" validate:"required"`
	ImagePullSecret string              `yaml:"imagePullSecret,omitempty"`
	ImagePullPolicy string              `yaml:"imagePullPolicy,omitempty" validate:"omitempty,oneof=Always Never IfNotPresent"`
	K8shelld        K8shelld            `yaml:"k8shelld" validate:"required"`
	Env             map[string]string   `yaml:"env,omitempty"`
	PortForwarding  []string            `yaml:"portForwarding,omitempty"`
	Network         Network             `yaml:"network" validate:"required"`
	Resources       Resources           `yaml:"resources" validate:"required"`
	Docker          Docker              `yaml:"docker" validate:"required"`
	Storages        map[string]Storage  `yaml:"storages" validate:"required,min=1,dive"`
	InitScripts     []map[string]string `yaml:"initScripts,omitempty"`
	ServiceAccount  string              `yaml:"serviceAccount,omitempty"`
}

// CustomBlueprint represents a custom blueprint configuration
type CustomBlueprint struct {
	Name           string              `yaml:"name,omitempty"`
	Template       string              `yaml:"template" validate:"required"`
	Shell          string              `yaml:"shell,omitempty"`
	Sudo           bool                `yaml:"sudo,omitempty"`
	Image          string              `yaml:"image,omitempty"`
	Env            map[string]string   `yaml:"env,omitempty"`
	PortForwarding []string            `yaml:"portForwarding,omitempty"`
	Network        Network             `yaml:"network,omitempty"`
	Resources      Resources           `yaml:"resources,omitempty"`
	Storages       map[string]Storage  `yaml:"storages,omitempty"`
	InitScripts    []map[string]string `yaml:"initScripts,omitempty"`
}

// K8shelld represents k8shelld configuration
type K8shelld struct {
	Image           string   `yaml:"image" validate:"required"`
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
	GroupID        int               `yaml:"groupId" validate:"min=0,max=65535"`
	SubGID         int               `yaml:"subgid" validate:"min=0"`
	ParentStorages bool              `yaml:"parentStorages"`
	ExtFiles       map[string]string `yaml:"extFiles,omitempty"`
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

// Values converts the Blueprint to Helm values
func (b *Blueprint) Values() (map[string]interface{}, error) {
	yamlBytes, err := yaml.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal blueprint to YAML: %w", err)
	}

	var values map[string]interface{}
	if err := yaml.Unmarshal(yamlBytes, &values); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML to map: %w", err)
	}

	return values, nil
}

func ValidateCustomBlueprint(blueprintYAML []byte) []string {
	var fullYAML map[string]interface{}
	if err := yaml.Unmarshal(blueprintYAML, &fullYAML); err != nil {
		return []string{
			fmt.Sprintf("Invalid YAML format: %v", err),
		}
	}

	var blueprintData, exists = fullYAML["blueprint"]
	if !exists {
		return []string{
			"Blueprint data is missing in the YAML",
		}
	}

	blueprintOnlyYAML, err := yaml.Marshal(blueprintData)
	if err != nil {
		return []string{
			fmt.Sprintf("Failed to process blueprint data: %v", err),
		}
	}

	var customBp CustomBlueprint
	decoder := yaml.NewDecoder(bytes.NewReader(blueprintOnlyYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&customBp); err != nil {
		return []string{
			fmt.Sprintf("Failed to decode blueprint: %v", err),
		}
	}

	validate := validator.New()
	if err := validate.Struct(customBp); err != nil {
		var validationErrors []string
		for _, err := range err.(validator.ValidationErrors) {
			validationErrors = append(validationErrors,
				fmt.Sprintf("Field '%s' failed validation: %s", err.Field(), err.Tag()))
		}
		return validationErrors
	}
	return nil
}
