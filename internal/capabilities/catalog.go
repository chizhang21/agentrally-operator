package capabilities

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

//go:embed catalog/*.yaml
var catalogFiles embed.FS

type runtimeDefinition struct {
	Family        string   `json:"family"`
	Category      string   `json:"category"`
	ImageContract string   `json:"imageContract"`
	Handlers      []string `json:"handlers"`
}
type storageDefinition struct {
	Type          string            `json:"type"`
	Provisioners  []string          `json:"provisioners"`
	Contains      []string          `json:"contains"`
	Suffixes      []string          `json:"suffixes"`
	ProtocolTypes map[string]string `json:"protocolTypes"`
	Legacy        bool              `json:"legacy"`
}
type networkDefinition struct {
	Provider               string            `json:"provider"`
	DaemonSets             []string          `json:"daemonSets"`
	Containers             []string          `json:"containers"`
	Status                 string            `json:"status"`
	MissingContainerStatus string            `json:"missingContainerStatus"`
	ConfigMap              string            `json:"configMap"`
	DisabledConfig         map[string]string `json:"disabledConfig"`
	DisabledArgs           []string          `json:"disabledArgs"`
	RequiredArgs           []string          `json:"requiredArgs"`
	MissingArgsStatus      string            `json:"missingArgsStatus"`
}
type recognitionCatalog struct {
	Runtimes         []runtimeDefinition `json:"runtimes"`
	Storage          []storageDefinition `json:"storage"`
	Network          []networkDefinition `json:"network"`
	runtimeByHandler map[string]runtimeDefinition
	networkByName    map[string]networkDefinition
	revision         string
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
	c.networkByName = map[string]networkDefinition{}
	families := map[string]bool{}
	for _, r := range c.Runtimes {
		if r.Family == "" || families[r.Family] {
			return fmt.Errorf("invalid or duplicate runtime family %q", r.Family)
		}
		families[r.Family] = true
		if !oneOf(r.Category, "Container", "UserSpaceKernel", "VirtualMachine", "WebAssembly") || !oneOf(r.ImageContract, "LinuxOCI", "WasmWASI") || (r.Category == "WebAssembly") != (r.ImageContract == "WasmWASI") {
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
		if !oneOf(r.Type, "NFS", "Block", "Local", "Unknown") || len(r.Provisioners)+len(r.Contains)+len(r.Suffixes) == 0 || (r.Legacy && r.Type != "NFS") {
			return fmt.Errorf("invalid storage signature %+v", r)
		}
		for _, token := range append(append(append([]string{}, r.Provisioners...), r.Contains...), r.Suffixes...) {
			if token == "" {
				return fmt.Errorf("empty storage matcher")
			}
		}
		for _, kind := range r.ProtocolTypes {
			if !oneOf(kind, "NFS", "Block", "Unknown") {
				return fmt.Errorf("invalid protocol type %q", kind)
			}
		}
	}
	for _, r := range c.Network {
		validStatuses := oneOf(r.Status, "", "Unsupported") &&
			oneOf(r.MissingContainerStatus, "", "Unsupported") &&
			oneOf(r.MissingArgsStatus, "", "Unknown", "Unsupported")
		missingConfigMap := len(r.DisabledConfig) > 0 && r.ConfigMap == ""
		if r.Provider == "" || len(r.DaemonSets) == 0 || !validStatuses || missingConfigMap {
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
	p := strings.ToLower(provisioner)
	for _, r := range builtins.Storage {
		if oneOf(p, r.Provisioners...) {
			return r, true
		}
		for _, token := range r.Contains {
			if strings.Contains(p, token) {
				return r, true
			}
		}
		for _, suffix := range r.Suffixes {
			if strings.HasSuffix(p, suffix) {
				return r, true
			}
		}
	}
	return storageDefinition{}, false
}
