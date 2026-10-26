package capabilities

import (
	"testing"

	v1 "agentrally/operator/api/v1alpha1"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStorageDiscoveryUsesNativeFilesystemParameters(t *testing.T) {
	for _, test := range []struct {
		name       string
		parameters map[string]string
		csi        bool
		kind, key  string
	}{
		{"standard", map[string]string{"csi.storage.k8s.io/fstype": "ext4"}, true, "Filesystem", "csi.storage.k8s.io/fstype"},
		{"legacy", map[string]string{"fsType": "xfs"}, true, "Filesystem", "fsType"},
		{"legacy case", map[string]string{"FsTyPe": "ext4"}, true, "Filesystem", "FsTyPe"},
		{"future filesystem", map[string]string{"csi.storage.k8s.io/fstype": "futurefs"}, true, "Filesystem", "csi.storage.k8s.io/fstype"},
		{"empty", map[string]string{"csi.storage.k8s.io/fstype": " "}, true, "Unknown", ""},
		{"conflicting", map[string]string{"csi.storage.k8s.io/fstype": "ext4", "fsType": "xfs"}, true, "Unknown", ""},
		{"duplicate legacy", map[string]string{"fstype": "ext4", "fsType": "ext4"}, true, "Unknown", ""},
		{"backend only", map[string]string{"protocol": "fc", "volumeType": "lun", "type": "block"}, true, "Unknown", ""},
		{"unidentified provisioner", map[string]string{"csi.storage.k8s.io/fstype": "ext4"}, false, "Unknown", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			sc := class("workspace", "future.csi.example")
			sc.Parameters = test.parameters
			objects := []client.Object{sc}
			if test.csi {
				objects = append(objects, csiRegistration(sc.Provisioner))
			}
			report := &v1.ClusterCapabilities{}
			observer := Observer{Reader: testClient(t, objects...)}
			observer.Refresh(t.Context(), report, All, true)
			observed := findStorage(t, report.Status, sc.Name)
			if observed.Type != test.kind || observed.Source != "NativeAPI" || (observed.Status == "Observed") != test.csi {
				t.Fatalf("classification and registration conflated: %+v", observed)
			}
			if test.key != "" && observed.Rule != "storageclass.parameters/"+test.key {
				t.Fatal("filesystem parameter provenance was lost", observed)
			}
			if test.csi && test.kind == "Unknown" && observed.Reason != "StorageTypeUnrecognized" {
				t.Fatal("interface uncertainty disappeared behind positive registration", observed)
			}
			found := false
			for _, capability := range report.Status.Capabilities {
				if capability.Name == "Filesystem" || capability.Name == "UnclassifiedStorage" {
					found = true
					if (capability.Name == "Filesystem") != (test.kind == "Filesystem") || capability.Status != observed.Status || capability.Resources[0].Reason != observed.Reason {
						t.Fatal("summary hid classification or registration evidence", capability)
					}
				}
			}
			if !found {
				t.Fatal("native storage evidence was omitted from the summary")
			}
		})
	}
}
