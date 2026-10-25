package capabilities

import v1 "arally/operator/api/v1alpha1"

func applyMappings(status *v1.ClusterCapabilitiesStatus, spec v1.ClusterCapabilitiesSpec, section Section) {
	if section == Runtime {
		for i := range status.Runtimes {
			observed := &status.Runtimes[i]
			for _, mapping := range spec.RuntimeMappings {
				if mapping.RuntimeClassName != observed.Name {
					continue
				}
				classification := runtimeClassification{Family: mapping.Family, Category: mapping.Category, ImageContract: mapping.ImageContract}
				classification.apply(observed, "AdministratorDeclared", "runtimeMappings/"+mapping.RuntimeClassName)
				runtimeEvidenceStatus(observed)
			}
		}
	}
	if section == Storage {
		for i := range status.StorageClasses {
			observed := &status.StorageClasses[i]
			for _, mapping := range spec.StorageMappings {
				if mapping.StorageClassName != observed.Name {
					continue
				}
				observed.Type = mapping.Type
				observed.Source, observed.Rule = "AdministratorDeclared", "storageMappings/"+mapping.StorageClassName
				storageEvidenceStatus(observed)
			}
		}
	}
}
