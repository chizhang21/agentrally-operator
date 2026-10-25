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
func nonCSIProvisioner(provisioner string) bool {
	rule, ok := storageRule(provisioner)
	return ok && rule.NonCSI
}
func classify(c storagev1.StorageClass) string {
	rule, ok := storageRule(c.Provisioner)
	if !ok {
		return "Unknown"
	}
	if len(rule.ParameterTypes) > 0 {
		if kind, ok := rule.ParameterTypes[c.Parameters[rule.Parameter]]; ok {
			return kind
		}
		return "Unknown"
	}
	return rule.Type
}

func storageDriverName(c storagev1.StorageClass) string {
	rule, _ := storageRule(c.Provisioner)
	if rule.NonCSI {
		return ""
	}
	if rule.DriverFromParameter {
		value := c.Parameters[rule.Parameter]
		if _, known := rule.ParameterTypes[value]; known {
			return value
		}
		return ""
	}
	return c.Provisioner
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
	declared := map[string]*storagev1.CSIDriver{}
	names := map[string]bool{}
	for name := range registeredNodes {
		names[name] = true
	}
	for _, driver := range drivers.Items {
		if driver.DeletionTimestamp == nil {
			declared[driver.Name] = &driver
			names[driver.Name] = true
		}
	}
	for name := range names {
		status.CSIDrivers = append(status.CSIDrivers, name)
	}
	sort.Strings(status.CSIDrivers)
	for _, name := range status.CSIDrivers {
		observed := v1.CSIDriverObservation{Name: name, Declared: declared[name] != nil, RegisteredNodes: registeredNodes[name]}
		if driver := declared[name]; driver != nil {
			observed.AttachRequired, observed.FSGroupPolicy = driver.Spec.AttachRequired, driver.Spec.FSGroupPolicy
			observed.VolumeLifecycleModes = slices.Clone(driver.Spec.VolumeLifecycleModes)
		}
		for key := range topologyKeys[name] {
			observed.TopologyKeys = append(observed.TopologyKeys, key)
		}
		sort.Strings(observed.TopologyKeys)
		sort.Slice(observed.VolumeLifecycleModes, func(i, j int) bool { return observed.VolumeLifecycleModes[i] < observed.VolumeLifecycleModes[j] })
		status.CSIDriverDetails = append(status.CSIDriverDetails, observed)
	}
	sort.Slice(classes.Items, func(i, j int) bool { return classes.Items[i].Name < classes.Items[j].Name })
	for _, class := range classes.Items {
		if class.DeletionTimestamp != nil {
			continue
		}
		kind := classify(class)
		driverName := storageDriverName(class)
		_, known := storageRule(class.Provisioner)
		if !known && declared[driverName] == nil && registeredNodes[driverName] == 0 {
			driverName = ""
		}
		c := v1.StorageObservation{Name: class.Name, Provisioner: class.Provisioner, Type: kind, Source: "NativeAPI", Default: isDefault(class),
			CSIDriver: driverName, DriverDeclared: declared[driverName] != nil, DriverRegistered: registeredNodes[driverName] > 0, AllowVolumeExpansion: class.AllowVolumeExpansion}
		if known {
			c.Source, c.Rule = "BuiltInCatalog", "storage/"+class.Provisioner
		}
		for _, term := range class.AllowedTopologies {
			c.AllowedTopologies = append(c.AllowedTopologies, *term.DeepCopy())
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
	case !c.DriverRegistered && !nonCSIProvisioner(c.Provisioner):
		c.Status, c.Reason = "Unknown", "CSIDriverNotRegistered"
	}
}
