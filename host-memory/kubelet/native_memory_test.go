package eviction

import (
	"slices"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

func nativeMemoryPod(name, class string, priority int32, request, limit string) *v1.Pod {
	pod := newPod(name, priority, []v1.Container{newContainer("application", newResourceList("", request, ""), newResourceList("", limit, ""))}, nil)
	if class != "" {
		pod.Spec.RuntimeClassName = &class
	}
	return pod
}

func nativeMemoryStats(values map[*v1.Pod]string) statsFunc {
	return func(pod *v1.Pod) (statsapi.PodStats, bool) {
		usage, ok := values[pod]
		if !ok {
			return statsapi.PodStats{}, false
		}
		return newPodMemoryStats(pod, resource.MustParse(usage)), true
	}
}

func TestNativeMemoryPressureProtectsServicesAtFullRequests(t *testing.T) {
	protected := nativeMemoryPod("protected-service", "", 1000, "100Mi", "2Gi")
	batch := nativeMemoryPod("full-request-guest", "kata-memory-canary", -100, "4Gi", "4Gi")
	pods := []*v1.Pod{protected, batch}
	// The native VM's actual host charges live in its bounded private hierarchy.
	// The legacy cAdvisor pod view may report zero, not a missing stats record.
	rankMemoryPressure(pods, nativeMemoryStats(map[*v1.Pod]string{protected: "500Mi", batch: "0"}))
	if !slices.Equal(pods, []*v1.Pod{batch, protected}) {
		t.Fatalf("memory eviction chose %s before the product best-effort guest", pods[0].Name)
	}
}

func TestNativeMemoryPressureKeepsStockOrderingInsideTiers(t *testing.T) {
	low := nativeMemoryPod("lower-priority-guest", "kata-memory-canary", -100, "4Gi", "4Gi")
	high := nativeMemoryPod("higher-priority-guest", "kata-memory-canary", -50, "4Gi", "4Gi")
	above := nativeMemoryPod("above-request-protected", "unknown-runtime", 1000, "100Mi", "2Gi")
	below := nativeMemoryPod("below-request-protected", "", -1000, "1Gi", "1Gi")
	pods := []*v1.Pod{below, high, above, low}
	rankMemoryPressure(pods, nativeMemoryStats(map[*v1.Pod]string{low: "100Mi", high: "200Mi", above: "500Mi", below: "200Mi"}))
	if !slices.Equal(pods, []*v1.Pod{low, high, above, below}) {
		t.Fatalf("tier or existing within-tier ordering changed: %s %s %s %s", pods[0].Name, pods[1].Name, pods[2].Name, pods[3].Name)
	}
}
