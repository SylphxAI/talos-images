package server

import (
	"context"
	"fmt"

	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/SylphxAI/talos-images/host-memory/guard"
	containerstore "github.com/containerd/containerd/v2/internal/cri/store/container"
	sandboxstore "github.com/containerd/containerd/v2/internal/cri/store/sandbox"
)

// CRI-side callers of the native host-memory adapter. Every function here is
// invoked from a patched upstream code path; none of them starts a daemon.

func (c *criService) startNativeMemory() {
	c.hostMemory = newNativeMemory(openNativeGuard, c.nativeInventory, c.nativeSandboxLive, func(reconcile func(context.Context) error, failure func(error)) (nativeObserver, error) {
		return startNativePressureWatcher(nativePressurePath, reconcile, nativeReconcileLimit, failure)
	})
}

// recoverNativeMemory runs after containerd sandbox/container metadata is
// loaded. It never fails CRI startup: inconsistent or unavailable memory state
// closes allocation while image, status, stop and remove APIs keep serving.
func (c *criService) recoverNativeMemory(ctx context.Context) {
	c.hostMemory.adopt(ctx)
}

func (c *criService) nativeInventory() []nativeSandbox {
	sandboxes := c.sandboxStore.List()
	out := make([]nativeSandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, nativeSandbox{ID: sb.ID, Handler: sb.RuntimeHandler, Parent: sb.Config.GetLinux().GetCgroupParent(), Live: c.nativeSandboxLive(sb.ID)})
	}
	return out
}

// nativeSandboxLive reports whether processes of a sandbox may still allocate:
// a ready sandbox or any running/unknown container. A created container cannot
// start without passing the native fence, so it is not live allocation.
func (c *criService) nativeSandboxLive(id string) bool {
	if sb, err := c.sandboxStore.Get(id); err == nil && sb.Status.Get().State == sandboxstore.StateReady {
		return true
	}
	for _, cntr := range c.containerStore.List() {
		if cntr.SandboxID != id {
			continue
		}
		switch cntr.Status.Get().State() {
		case runtime.ContainerState_CONTAINER_RUNNING, runtime.ContainerState_CONTAINER_UNKNOWN:
			return true
		}
	}
	return false
}

func nativeSandboxResources(sb sandboxstore.Sandbox) (*runtime.LinuxContainerResources, *runtime.LinuxContainerResources) {
	status := sb.Status.Get()
	resources, overhead := sb.Config.GetLinux().GetResources(), sb.Config.GetLinux().GetOverhead()
	if status.Resources != nil {
		resources = status.Resources.GetLinux()
	}
	if status.Overhead != nil {
		overhead = status.Overhead.GetLinux()
	}
	return resources, overhead
}

// nativePodUpdate fences UpdatePodSandboxResources before NRI, metadata or the
// sandbox controller can apply a larger pod envelope.
func (c *criService) nativePodUpdate(ctx context.Context, sb sandboxstore.Sandbox, resources, overhead *runtime.LinuxContainerResources) error {
	oldResources, oldOverhead := nativeSandboxResources(sb)
	return c.hostMemory.podUpdate(ctx, sb.ID, oldResources, oldOverhead, resources, overhead)
}

func nativeSpecMemoryLimit(spec *runtimespec.Spec) (int64, bool) {
	if spec == nil || spec.Linux == nil || spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil || spec.Linux.Resources.Memory.Limit == nil {
		return 0, false
	}
	return *spec.Linux.Resources.Memory.Limit, true
}

// nativeContainerResize runs inside the container's update transaction,
// including NRI-adjusted requests, before metadata or the task changes.
// Unchanged memory (CPU-only, cpuset, NRI no-op) returns without any memory
// check, so existing unlimited contexts keep working.
func (c *criService) nativeContainerResize(ctx context.Context, cntr containerstore.Container, oldSpec, newSpec *runtimespec.Spec) error {
	oldLimit, oldSet := nativeSpecMemoryLimit(oldSpec)
	newLimit, newSet := nativeSpecMemoryLimit(newSpec)
	if oldSet == newSet && oldLimit == newLimit {
		return nil
	}

	sb, err := c.sandboxStore.Get(cntr.SandboxID)
	if err != nil {
		return err
	}
	contract, err := c.hostMemory.admitted(ctx, sb.ID)
	if err != nil {
		return err
	}
	if contract.Mode == guard.Observed {
		// No invented aggregate of finite siblings: retain the original kernel caps
		// and revalidate ownership/mode before the actual container update.
		return c.hostMemory.fence(ctx, sb.ID)
	}
	if !newSet {
		return fmt.Errorf("container %s cannot remove its memory limit inside a finite envelope", cntr.ID)
	}
	_, overhead := nativeSandboxResources(sb)
	var others []int64
	for _, other := range c.containerStore.List() {
		if other.SandboxID != sb.ID || other.ID == cntr.ID {
			continue
		}
		status := other.Status.Get()
		if status.State() == runtime.ContainerState_CONTAINER_EXITED {
			continue
		}
		limit := other.Config.GetLinux().GetResources().GetMemoryLimitInBytes()
		if status.Resources != nil && status.Resources.GetLinux() != nil {
			limit = status.Resources.GetLinux().GetMemoryLimitInBytes()
		}
		others = append(others, limit)
	}
	return nativeContainerFits(contract.Envelope, newLimit, overhead.GetMemoryLimitInBytes(), others)
}
