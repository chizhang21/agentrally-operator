package capabilities

// NetworkDaemonSet identifies catalog inputs. Unknown installations remain
// Unknown and cannot turn a provider-scoped negative into a global conclusion.
func NetworkDaemonSet(name string) bool {
	_, found := builtins.networkByName[name]
	return found
}

func NetworkConfigMap(name string) bool {
	for _, rule := range builtins.Network {
		if rule.ConfigMap == name {
			return true
		}
	}
	return false
}
