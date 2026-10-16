package containerdshim

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/resourcecontrol"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/compatoci"
	"golang.org/x/sys/unix"
)

// Test seams for the kernel paths; production uses the host's own.
var (
	nativeCgroupRoot  = "/sys/fs/cgroup"
	nativeOOMScoreAdj = "/proc/self/oom_score_adj"
)

func nativeRead(dir *os.File, name string) (string, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var data [64]byte
	n, err := file.Read(data[:])
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data[:n])), nil
}

// nativeShimLaunch runs in the short-lived StartShim launcher before it forks
// the long-lived shim. It returns the cgroup directory FD for the child's
// CLONE_INTO_CGROUP. For the best-effort tier it raises only this launcher's
// own oom_score_adj; the shim, VMM and helpers inherit it. Protected scores
// are never written.
func nativeShimLaunch(bundle, id string) (*os.File, error) {
	spec, err := compatoci.ParseConfigJSON(bundle)
	if err != nil {
		return nil, err
	}
	if spec.Linux == nil {
		return nil, fmt.Errorf("Linux cgroup contract required for native shim launch")
	}
	parent, bestEffort, err := katautils.NativeParent(spec.Linux.CgroupsPath, id)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(nativeCgroupRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(fd), "cgroup-root")
	for _, part := range strings.Split(strings.TrimPrefix(parent, "/"), "/") {
		next, err := unix.Openat(int(dir.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		dir.Close()
		if err != nil {
			return nil, fmt.Errorf("native private parent %s: %w", parent, err)
		}
		dir = os.NewFile(uintptr(next), part)
	}
	defer dir.Close()
	// Guard installs finite limits on both tiers' private parents. Swap and
	// group-OOM containment are best-effort-leaf requirements only.
	for _, name := range []string{"memory.high", "memory.max"} {
		value, err := nativeRead(dir, name)
		if err != nil {
			return nil, err
		}
		if finite, err := strconv.ParseUint(value, 10, 64); err != nil || finite == 0 {
			return nil, fmt.Errorf("native private parent requires positive finite %s, got %q", name, value)
		}
	}
	if bestEffort {
		for name, want := range map[string]string{"memory.swap.max": "0", "memory.oom.group": "1"} {
			if value, err := nativeRead(dir, name); err != nil || value != want {
				return nil, fmt.Errorf("best-effort parent %s must be %s (got %q): %v", name, want, value, err)
			}
		}
		if err := os.WriteFile(nativeOOMScoreAdj, []byte("1000"), 0); err != nil {
			return nil, fmt.Errorf("raise best-effort shim oom_score_adj: %w", err)
		}
	}
	if err := unix.Mkdirat(int(dir.Fd()), resourcecontrol.NativeShimLeaf, 0o750); err != nil && err != unix.EEXIST {
		return nil, err
	}
	leaf, err := unix.Openat(int(dir.Fd()), resourcecontrol.NativeShimLeaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(leaf), "native-shim-cgroup"), nil
}
