package containerdshim

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kata-containers/kata-containers/src/runtime/pkg/resourcecontrol"
)

type nativeShimFixture struct {
	root, bundle, oom string
}

func newNativeShimFixture(t *testing.T) *nativeShimFixture {
	t.Helper()
	dir := t.TempDir()
	f := &nativeShimFixture{root: filepath.Join(dir, "cgroup"), bundle: filepath.Join(dir, "bundle"), oom: filepath.Join(dir, "oom_score_adj")}
	for _, d := range []string{f.root, f.bundle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.oom, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldRoot, oldOOM := nativeCgroupRoot, nativeOOMScoreAdj
	nativeCgroupRoot, nativeOOMScoreAdj = f.root, f.oom
	t.Cleanup(func() { nativeCgroupRoot, nativeOOMScoreAdj = oldRoot, oldOOM })
	return f
}

func (f *nativeShimFixture) sandbox(t *testing.T, cgroupsPath string) {
	t.Helper()
	spec := fmt.Sprintf(`{"ociVersion":"1.0.2","process":{"cwd":"/","capabilities":{}},"linux":{"cgroupsPath":%q}}`, cgroupsPath)
	if err := os.WriteFile(filepath.Join(f.bundle, "config.json"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *nativeShimFixture) parent(t *testing.T, parent string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(f.root, parent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func nativeShimDigest(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (f *nativeShimFixture) oomScore(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.oom)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestNativeShimProtectedLaunchesInsidePrivateParentWithoutBestEffortFlags(t *testing.T) {
	f := newNativeShimFixture(t)
	parent := "/kubepods/burstable/poda/native-" + nativeShimDigest("sb")
	f.sandbox(t, parent+"/sb")
	f.parent(t, parent, map[string]string{"memory.high": "1073741824", "memory.max": "1073741824"})
	leaf, err := nativeShimLaunch(f.bundle, "sb")
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if leaf.Name() != "native-shim-cgroup" {
		t.Fatal("unexpected cgroup handle")
	}
	if _, err := os.Stat(filepath.Join(f.root, parent, resourcecontrol.NativeShimLeaf)); err != nil {
		t.Fatal("shim leaf not created below the private parent", err)
	}
	if f.oomScore(t) != "0" {
		t.Fatal("protected launcher changed oom_score_adj")
	}
}

func TestNativeShimBestEffortRequiresContainmentAndRaisesOnlyLauncherScore(t *testing.T) {
	f := newNativeShimFixture(t)
	parent := "/sylphx-besteffort/pod-" + nativeShimDigest("sb")
	f.sandbox(t, parent+"/sb")
	f.parent(t, parent, map[string]string{"memory.high": "4096", "memory.max": "8192", "memory.swap.max": "0", "memory.oom.group": "0"})
	if _, err := nativeShimLaunch(f.bundle, "sb"); err == nil {
		t.Fatal("best-effort shim launched without per-pod group OOM containment")
	}
	if f.oomScore(t) != "0" {
		t.Fatal("refused launch changed oom_score_adj")
	}
	f.parent(t, parent, map[string]string{"memory.oom.group": "1"})
	leaf, err := nativeShimLaunch(f.bundle, "sb")
	if err != nil {
		t.Fatal(err)
	}
	leaf.Close()
	if f.oomScore(t) != "1000" {
		t.Fatal("best-effort launcher did not raise its inherited oom_score_adj")
	}
}

func TestNativeShimRefusesUnboundedOrForeignPlacement(t *testing.T) {
	f := newNativeShimFixture(t)
	protected := "/kubepods/burstable/poda/native-" + nativeShimDigest("sb")
	f.parent(t, protected, map[string]string{"memory.high": "max", "memory.max": "1073741824"})
	f.sandbox(t, protected+"/sb")
	if _, err := nativeShimLaunch(f.bundle, "sb"); err == nil {
		t.Fatal("shim launched below an unbounded private parent")
	}
	f.parent(t, protected, map[string]string{"memory.high": "1073741824"})
	for _, path := range []string{
		"/kubepods/burstable/poda/sb",
		"/kubepods/burstable/poda/native-" + nativeShimDigest("other") + "/sb",
		protected + "/other",
		"kubepods-burstable.slice:cri-containerd:sb",
	} {
		f.sandbox(t, path)
		if _, err := nativeShimLaunch(f.bundle, "sb"); err == nil {
			t.Fatalf("shim launched at foreign placement %q", path)
		}
	}
	if _, err := os.Stat(filepath.Join(f.root, protected, resourcecontrol.NativeShimLeaf)); !os.IsNotExist(err) {
		t.Fatal("refused launches created a shim cgroup")
	}
}

func TestNativeShimObservedProtectedRetainsUncappedAncestryAndScore(t *testing.T) {
	f := newNativeShimFixture(t)
	parent := "/kubepods/burstable/poda/native-" + nativeShimDigest("sb")
	f.sandbox(t, parent+"/sb")
	f.parent(t, parent, map[string]string{"memory.high": "max", "memory.max": "max"})
	leaf, err := nativeShimLaunch(f.bundle, "sb")
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if f.oomScore(t) != "0" {
		t.Fatal("observed protected inherited BE OOM adjustment")
	}
	if _, err = os.Stat(filepath.Join(f.root, parent, resourcecontrol.NativeShimLeaf)); err != nil {
		t.Fatal("shim escaped observed private parent", err)
	}
	for _, name := range []string{"memory.high", "memory.max"} {
		data, err := os.ReadFile(filepath.Join(f.root, parent, name))
		if err != nil || string(data) != "max\n" {
			t.Fatal("protected cap was changed", name, err)
		}
	}
}
func TestNativeShimBestEffortCannotUseObservedMode(t *testing.T) {
	f := newNativeShimFixture(t)
	parent := "/sylphx-besteffort/pod-" + nativeShimDigest("sb")
	f.sandbox(t, parent+"/sb")
	f.parent(t, parent, map[string]string{"memory.high": "max", "memory.max": "max", "memory.swap.max": "0", "memory.oom.group": "1"})
	if _, err := nativeShimLaunch(f.bundle, "sb"); err == nil {
		t.Fatal("BE escaped finite host envelope")
	}
	if f.oomScore(t) != "0" {
		t.Fatal("refused launch changed OOM score")
	}
}
