package capabilities

import appsv1 "k8s.io/api/apps/v1"

// NetworkProviderName identifies catalog candidates without making API reads or
// evaluating namespace trust and effective policy configuration.
func NetworkProviderName(daemon *appsv1.DaemonSet) string {
	if daemon == nil {
		return ""
	}
	rule, _ := networkRule(daemon)
	return rule.Provider
}
