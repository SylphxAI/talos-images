package eviction

import v1 "k8s.io/api/core/v1"

// This admission-controlled RuntimeClass is the same best-effort binding used
// by the native CRI guard. Unknown bindings retain protected treatment. It is
// not Kubernetes BestEffort/Burstable QoS and never comes from a tenant label.
func nativeMemoryTier(left, right *v1.Pod) int {
	bestEffort := func(pod *v1.Pod) bool {
		return pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == "kata-memory-canary"
	}
	a, b := bestEffort(left), bestEffort(right)
	if a == b {
		return 0
	}
	if a {
		return -1
	}
	return 1
}
