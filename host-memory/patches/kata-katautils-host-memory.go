package katautils

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/kata-containers/kata-containers/src/runtime/pkg/oci"
	vc "github.com/kata-containers/kata-containers/src/runtime/virtcontainers"
)

// Native host-memory placement for Kata sandboxes launched by the patched
// containerd CRI. containerd's podsandbox controller sets the sandbox
// container's OCI cgroupsPath to "<private parent>/<sandbox id>", where the
// private parent is the runtime-owned child admitted by Guard:
//   - best-effort: /sylphx-besteffort/pod-<sha256 sandbox id>
//   - protected:   /kubepods/.../native-<sha256 sandbox id>
const (
	nativeAggregate = "/sylphx-besteffort"
	nativePodRoot   = "/kubepods"
)

// NativeParent returns the admitted private parent and its tier. Any other
// placement, including systemd syntax, is refused: such a sandbox would start
// outside its reserved envelope.
func NativeParent(cgroupsPath, sandboxID string) (parent string, bestEffort bool, err error) {
	if sandboxID == "" || strings.ContainsAny(cgroupsPath, ":\x00") || !path.IsAbs(cgroupsPath) || path.Clean(cgroupsPath) != cgroupsPath {
		return "", false, fmt.Errorf("native host memory requires a clean cgroupfs sandbox path, got %q", cgroupsPath)
	}
	if path.Base(cgroupsPath) != sandboxID {
		return "", false, fmt.Errorf("sandbox cgroup %q does not belong to sandbox %s", cgroupsPath, sandboxID)
	}
	sum := sha256.Sum256([]byte(sandboxID))
	digest := hex.EncodeToString(sum[:])
	parent = path.Dir(cgroupsPath)
	if parent == nativeAggregate+"/pod-"+digest {
		return parent, true, nil
	}
	if path.Base(parent) == "native-"+digest && strings.HasPrefix(parent, nativePodRoot+"/") && path.Dir(parent) != nativePodRoot {
		return parent, false, nil
	}
	return "", false, fmt.Errorf("sandbox cgroup %q is outside its native host-memory parent", cgroupsPath)
}

// ValidateNativeFactory refuses VM factory, template and VM cache paths before
// HandleFactory can hand the sandbox a VM created outside its envelope.
func ValidateNativeFactory(config oci.FactoryConfig) error {
	if config.Template || config.VMCacheNumber != 0 || config.VMCacheEndpoint != "" {
		return fmt.Errorf("native host memory does not support VM factory, template or cache")
	}
	return nil
}

// ValidateNativeSandbox checks the effective sandbox config after OCI
// annotation overrides: a fresh local Cloud Hypervisor VM whose VMM, shim and
// helpers all stay in the sandbox cgroup inside the private parent.
func ValidateNativeSandbox(cgroupsPath, sandboxID string, config vc.SandboxConfig) error {
	if _, _, err := NativeParent(cgroupsPath, sandboxID); err != nil {
		return err
	}
	h := config.HypervisorConfig
	switch {
	case !config.SandboxCgroupOnly:
		return fmt.Errorf("native host memory requires sandbox_cgroup_only")
	case config.HypervisorType != vc.ClhHypervisor:
		return fmt.Errorf("native host memory requires local Cloud Hypervisor, got %q", config.HypervisorType)
	case h.JailerPath != "":
		return fmt.Errorf("native host memory does not support a jailer")
	case h.BootFromTemplate || h.BootToBeTemplate || h.VMid != "":
		return fmt.Errorf("native host memory requires a fresh VM, not a template or factory VM")
	}
	return nil
}
