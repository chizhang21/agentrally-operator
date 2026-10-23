package capabilities

import (
	"context"
	"slices"
	"sort"

	v1 "arally/operator/api/v1alpha1"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func isDefault(c storagev1.StorageClass) bool {
	return c.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" || c.Annotations["storageclass.beta.kubernetes.io/is-default-class"] == "true"
}
func legacyNFS(provisioner string) bool {
	rule, ok := storageRule(provisioner)
	return ok && rule.Legacy
}
func classify(c storagev1.StorageClass) string {
	rule, ok := storageRule(c.Provisioner)
	if !ok {
		return "Unknown"
	}
	if len(rule.ProtocolTypes) > 0 {
		if kind, ok := rule.ProtocolTypes[c.Parameters["protocol"]]; ok {
			return kind
		}
		return "Unknown"
	}
	return rule.Type
}

func discoverStorage(ctx context.Context, reader client.Reader, status *v1.ClusterCapabilitiesStatus) error {
	classes := &storagev1.StorageClassList{}
	if err := reader.List(ctx, classes); err != nil {
		return err
	}
	drivers := &storagev1.CSIDriverList{}
	if err := reader.List(ctx, drivers); err != nil {
		return err
	}
	nodes := &storagev1.CSINodeList{}
	if err := reader.List(ctx, nodes); err != nil {
		return err
	}
	sort.Slice(drivers.Items, func(i, j int) bool { return drivers.Items[i].Name < drivers.Items[j].Name })
	registeredNodes := map[string]int32{}
	topologyKeys := map[string]map[string]bool{}
	for _, node := range nodes.Items {
		if node.DeletionTimestamp != nil {
			continue
		}
		for _, registered := range node.Spec.Drivers {
			registeredNodes[registered.Name]++
			if topologyKeys[registered.Name] == nil {
				topologyKeys[registered.Name] = map[string]bool{}
			}
			for _, key := range registered.TopologyKeys {
				topologyKeys[registered.Name][key] = true
			}
		}
	}
	installed := map[string]bool{}
	for _, driver := range drivers.Items {
		if driver.DeletionTimestamp == nil {
			installed[driver.Name] = true
			status.CSIDrivers = append(status.CSIDrivers, driver.Name)
			observed := v1.CSIDriverObservation{Name: driver.Name, AttachRequired: driver.Spec.AttachRequired, FSGroupPolicy: driver.Spec.FSGroupPolicy, VolumeLifecycleModes: slices.Clone(driver.Spec.VolumeLifecycleModes), RegisteredNodes: registeredNodes[driver.Name]}
			for key := range topologyKeys[driver.Name] {
				observed.TopologyKeys = append(observed.TopologyKeys, key)
			}
			sort.Strings(observed.TopologyKeys)
			sort.Slice(observed.VolumeLifecycleModes, func(i, j int) bool { return observed.VolumeLifecycleModes[i] < observed.VolumeLifecycleModes[j] })
			status.CSIDriverDetails = append(status.CSIDriverDetails, observed)
		}
	}
	sort.Strings(status.CSIDrivers)
	sort.Slice(classes.Items, func(i, j int) bool { return classes.Items[i].Name < classes.Items[j].Name })
	for _, class := range classes.Items {
		if class.DeletionTimestamp != nil {
			continue
		}
		kind := classify(class)
		c := v1.StorageObservation{Name: class.Name, Provisioner: class.Provisioner, Type: kind, Source: "NativeAPI", Default: isDefault(class), DriverRegistered: installed[class.Provisioner], AllowVolumeExpansion: class.AllowVolumeExpansion}
		if _, known := storageRule(class.Provisioner); known {
			c.Source, c.Rule = "BuiltInCatalog", "storage/"+class.Provisioner
		}
		for _, term := range class.AllowedTopologies {
			c.AllowedTopologies = append(c.AllowedTopologies, *term.DeepCopy())
		}
		if class.VolumeBindingMode != nil {
			c.VolumeBindingMode = *class.VolumeBindingMode
		}
		if class.ReclaimPolicy != nil {
			c.ReclaimPolicy = string(*class.ReclaimPolicy)
		}
		storageEvidenceStatus(&c)
		status.StorageClasses = append(status.StorageClasses, c)
	}
	return nil
}

func storageEvidenceStatus(c *v1.StorageObservation) {
	c.Status, c.Reason = "Observed", ""
	switch {
	case c.Type == "Unknown":
		c.Status, c.Reason = "Unknown", "StorageTypeUnrecognized"
	case c.Type == "Local":
		c.Reason = "LocalStorageIsNotDurableBlock"
	case !c.DriverRegistered && !legacyNFS(c.Provisioner):
		c.Status, c.Reason = "Unknown", "CSIDriverNotRegistered"
	}
}
