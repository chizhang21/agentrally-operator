package capabilities

import (
	"context"
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
	installed := map[string]bool{}
	for _, driver := range drivers.Items {
		if driver.DeletionTimestamp == nil {
			installed[driver.Name] = true
			status.CSIDrivers = append(status.CSIDrivers, driver.Name)
		}
	}
	sort.Strings(status.CSIDrivers)
	sort.Slice(classes.Items, func(i, j int) bool { return classes.Items[i].Name < classes.Items[j].Name })
	for _, class := range classes.Items {
		if class.DeletionTimestamp != nil {
			continue
		}
		kind := classify(class)
		c := v1.StorageObservation{Name: class.Name, Provisioner: class.Provisioner, Type: kind, Source: "Provisioner", Default: isDefault(class), DriverRegistered: installed[class.Provisioner], Status: "Observed"}
		if class.VolumeBindingMode != nil {
			c.VolumeBindingMode = *class.VolumeBindingMode
		}
		if class.ReclaimPolicy != nil {
			c.ReclaimPolicy = string(*class.ReclaimPolicy)
		}
		switch {
		case kind == "Unknown":
			c.Status = "Unknown"
			c.Reason = "StorageTypeUnrecognized"
		case kind == "Local":
			c.Reason = "LocalStorageIsNotDurableBlock"
		case !c.DriverRegistered && !legacyNFS(class.Provisioner):
			c.Status = "Unknown"
			c.Reason = "CSIDriverNotRegistered"
		}
		status.StorageClasses = append(status.StorageClasses, c)
	}
	return nil
}
