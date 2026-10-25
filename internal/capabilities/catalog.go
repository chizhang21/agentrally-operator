package capabilities

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

//go:embed catalog/*.yaml
var catalogFiles embed.FS

type runtimeDefinition struct {
	runtimeClassification `json:",inline"`
	Handlers              []string          `json:"handlers"`
	Labels                map[string]string `json:"labels"`
	NodeSelector          map[string]string `json:"nodeSelector"`
}
type storageDefinition struct {
	Type                string            `json:"type"`
	Provisioners        []string          `json:"provisioners"`
	Suffixes            []string          `json:"suffixes"`
	Parameter           string            `json:"parameter"`
	ParameterTypes      map[string]string `json:"parameterTypes"`
	DriverFromParameter bool              `json:"driverFromParameter"`
	NonCSI              bool              `json:"nonCSI"`
}
type networkDefinition struct {
	Provider               string            `json:"provider"`
	DaemonSets             []string          `json:"daemonSets"`
	Containers             []string          `json:"containers"`
	Labels                 map[string]string `json:"labels"`
	Status                 string            `json:"status"`
	MissingContainerStatus string            `json:"missingContainerStatus"`
	BooleanOption          string            `json:"booleanOption"`
	DefaultBooleanValue    *bool             `json:"defaultBooleanValue"`
}
type recognitionCatalog struct {
	Runtimes             []runtimeDefinition `json:"runtimes"`
	Storage              []storageDefinition `json:"storage"`
	Network              []networkDefinition `json:"network"`
	runtimeByHandler     map[string]runtimeDefinition
	storageByProvisioner map[string]storageDefinition
	networkByName        map[string]networkDefinition
	revision             string
}

var builtins = mustLoadCatalog()

func mustLoadCatalog() recognitionCatalog {
	var catalog recognitionCatalog
	hash := sha256.New()
	for _, name := range []string{"runtimes", "storage", "network"} {
		data, err := catalogFiles.ReadFile("catalog/" + name + ".yaml")
		if err != nil {
			panic(err)
		}
		if err := yaml.UnmarshalStrict(data, &catalog); err != nil {
			panic(fmt.Errorf("embedded %s catalog: %w", name, err))
		}
		hash.Write(data)
	}
	if err := catalog.validate(); err != nil {
		panic(err)
	}
	catalog.revision = hex.EncodeToString(hash.Sum(nil))
	return catalog
}

func (c *recognitionCatalog) validate() error {
	if len(c.Runtimes) == 0 || len(c.Storage) == 0 || len(c.Network) == 0 {
		return fmt.Errorf("embedded capability catalogs must not be empty")
	}
	c.runtimeByHandler = map[string]runtimeDefinition{}
	c.storageByProvisioner = map[string]storageDefinition{}
	c.networkByName = map[string]networkDefinition{}
	families := map[string]bool{}
	for _, r := range c.Runtimes {
		if r.Family == "" || families[r.Family] {
			return fmt.Errorf("invalid or duplicate runtime family %q", r.Family)
		}
		families[r.Family] = true
		if !r.valid() || len(r.Handlers)+len(r.Labels) == 0 || (len(r.NodeSelector) > 0 && len(r.Labels) == 0) {
			return fmt.Errorf("invalid runtime contract for %s", r.Family)
		}
		for _, handler := range r.Handlers {
			if handler == "" || handler != strings.ToLower(handler) {
				return fmt.Errorf("invalid runtime handler %q", handler)
			}
			if _, exists := c.runtimeByHandler[handler]; exists {
				return fmt.Errorf("duplicate runtime handler %q", handler)
			}
			c.runtimeByHandler[handler] = r
		}
	}
	for _, r := range c.Storage {
		validType := oneOf(r.Type, "Filesystem", "Block", "Unknown")
		validNonCSI := !r.NonCSI || oneOf(r.Type, "Filesystem", "Unknown")
		missingParameter := len(r.ParameterTypes) > 0 && r.Parameter == ""
		missingDriverTypes := r.DriverFromParameter && len(r.ParameterTypes) == 0
		if !validType || !validNonCSI || len(r.Provisioners)+len(r.Suffixes) == 0 || missingParameter || missingDriverTypes {
			return fmt.Errorf("invalid storage signature %+v", r)
		}
		for _, token := range append(append([]string{}, r.Provisioners...), r.Suffixes...) {
			if token == "" {
				return fmt.Errorf("empty storage matcher")
			}
		}
		for _, name := range r.Provisioners {
			if _, exists := c.storageByProvisioner[name]; exists {
				return fmt.Errorf("duplicate storage provisioner %q", name)
			}
			c.storageByProvisioner[name] = r
		}
		for _, suffix := range r.Suffixes {
			if !strings.HasPrefix(suffix, ".") {
				return fmt.Errorf("storage suffix must have a namespace boundary: %q", suffix)
			}
		}
		for _, kind := range r.ParameterTypes {
			if !oneOf(kind, "Filesystem", "Block", "Unknown") {
				return fmt.Errorf("invalid storage parameter type %q", kind)
			}
		}
	}
	for _, r := range c.Network {
		validStatuses := oneOf(r.Status, "", "Unknown", "Unsupported") && oneOf(r.MissingContainerStatus, "", "Unknown", "Unsupported")
		if r.Provider == "" || len(r.DaemonSets) == 0 || !validStatuses || r.DefaultBooleanValue != nil && r.BooleanOption == "" {
			return fmt.Errorf("invalid network signature %+v", r)
		}
		for _, name := range r.DaemonSets {
			if name == "" {
				return fmt.Errorf("empty network matcher")
			}
			if _, exists := c.networkByName[name]; exists {
				return fmt.Errorf("duplicate network signature %q", name)
			}
			c.networkByName[name] = r
		}
	}
	return nil
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func storageRule(provisioner string) (storageDefinition, bool) {
	if rule, found := builtins.storageByProvisioner[provisioner]; found {
		return rule, true
	}
	for _, r := range builtins.Storage {
		for _, suffix := range r.Suffixes {
			if strings.HasSuffix(provisioner, suffix) && len(provisioner) > len(suffix) {
				return r, true
			}
		}
	}
	return storageDefinition{}, false
}

func networkRule(ds *appsv1.DaemonSet) (networkDefinition, bool) {
	if rule, found := builtins.networkByName[ds.Name]; found {
		return rule, true
	}
	for _, rule := range builtins.Network {
		if len(rule.Labels) == 0 {
			continue
		}
		matches := func(labels map[string]string) bool {
			for key, value := range rule.Labels {
				if observed, exists := labels[key]; !exists || observed != value {
					return false
				}
			}
			return true
		}
		if matches(ds.Labels) || matches(ds.Spec.Template.Labels) {
			for _, container := range ds.Spec.Template.Spec.Containers {
				if oneOf(container.Name, rule.Containers...) {
					return rule, true
				}
			}
		}
	}
	return networkDefinition{}, false
}
