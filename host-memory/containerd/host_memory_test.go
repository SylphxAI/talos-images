package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SylphxAI/talos-images/host-memory/guard"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerstore "github.com/containerd/containerd/v2/internal/cri/store/container"
)

// nativeContracts is Guard's public contract state machine (Admitted ->
// Released tombstone -> Forget), without kernel I/O. It lets these tests drive
// the adapter's lifecycle ordering against the exact Guard error semantics.
type nativeContracts struct {
	mu        sync.Mutex
	contracts map[string]guard.Contract
	deny      error
	held      map[string]bool
	reserves  []uint64
}

func newNativeContracts() *nativeContracts {
	return &nativeContracts{contracts: map[string]guard.Contract{}, held: map[string]bool{}}
}

func (d *nativeContracts) Reserve(_ context.Context, id string, tier guard.Tier, envelope, floor uint64, original string) (guard.Contract, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deny != nil {
		return guard.Contract{}, d.deny
	}
	if old, ok := d.contracts[id]; ok && (old.State != guard.StateAdmitted || old.Tier != tier || old.Original != original) {
		return guard.Contract{}, errors.New("contract identity/state changed")
	}
	c := guard.Contract{ID: id, Tier: tier, State: guard.StateAdmitted, Envelope: envelope, Floor: floor, Original: original, Parent: nativePrivateParent(id, tier, original)}
	d.contracts[id] = c
	d.reserves = append(d.reserves, envelope)
	return c, nil
}

func (d *nativeContracts) Fence(_ context.Context, id string) (guard.Contract, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.contracts[id]
	if !ok || c.State != guard.StateAdmitted {
		return guard.Contract{}, errors.New("no admitted contract")
	}
	if d.deny != nil {
		return guard.Contract{}, d.deny
	}
	return c, nil
}

func (d *nativeContracts) Release(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.contracts[id]
	if !ok {
		return nil
	}
	if d.held[id] {
		return fmt.Errorf("sandbox %s populated: %w", id, guard.ErrHeld)
	}
	c.State = guard.StateReleased
	d.contracts[id] = c
	return nil
}

func (d *nativeContracts) Forget(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.contracts[id]
	if !ok {
		return nil
	}
	if c.State != guard.StateReleased {
		return errors.New("forget before release")
	}
	delete(d.contracts, id)
	return nil
}

func (d *nativeContracts) Contract(id string) (guard.Contract, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.contracts[id]
	return c, ok
}

func (d *nativeContracts) Contracts() []guard.Contract {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]guard.Contract, 0, len(d.contracts))
	for _, c := range d.contracts {
		out = append(out, c)
	}
	return out
}

func (d *nativeContracts) Reconcile(context.Context) error { return nil }
func (d *nativeContracts) Close() error                    { return nil }

type nativeHarness struct {
	memory    *nativeMemory
	driver    *nativeContracts
	sandboxes []nativeSandbox
	live      map[string]bool
	openErr   error
}

func newNativeHarness(t *testing.T) *nativeHarness {
	t.Helper()
	h := &nativeHarness{driver: newNativeContracts(), live: map[string]bool{}}
	h.memory = newNativeMemory(func() (memoryDriver, error) {
		if h.openErr != nil {
			return nil, h.openErr
		}
		return h.driver, nil
	}, func() []nativeSandbox { return h.sandboxes }, func(id string) bool { return h.live[id] }, nil)
	return h
}

func nativePod(parent string, limit, overhead int64) *runtime.PodSandboxConfig {
	return &runtime.PodSandboxConfig{Linux: &runtime.LinuxPodSandboxConfig{CgroupParent: parent,
		Resources: &runtime.LinuxContainerResources{MemoryLimitInBytes: limit}, Overhead: &runtime.LinuxContainerResources{MemoryLimitInBytes: overhead}}}
}

func nativeLimits(memory int64) *runtime.LinuxContainerResources {
	return &runtime.LinuxContainerResources{MemoryLimitInBytes: memory, CpuShares: 2}
}

func TestNativeEnvelopeCountsOverheadOnceAndRefusesUnlimited(t *testing.T) {
	envelope, floor, err := nativeEnvelope(nativeLimits(1024), nativeLimits(256))
	if err != nil || envelope != 1280 || floor != 1280 {
		t.Fatalf("envelope=%d floor=%d err=%v", envelope, floor, err)
	}
	for _, pair := range [][2]int64{{0, 0}, {-1, 0}, {1, -1}, {math.MaxInt64, 1}} {
		if _, _, err := nativeEnvelope(nativeLimits(pair[0]), nativeLimits(pair[1])); err == nil {
			t.Fatalf("admitted invalid or unlimited envelope %v", pair)
		}
	}
	if _, _, err := nativeEnvelope(nil, nil); err == nil {
		t.Fatal("pod without kubelet resources admitted as finite")
	}
}

func TestNativeTierAndRuntimeBindingCannotEscape(t *testing.T) {
	for _, handler := range []string{"", "kata", "runc", "BestEffort", "kata-memory-canary-tenant"} {
		if nativeTier(handler) != guard.Protected {
			t.Fatalf("handler %q was not protected", handler)
		}
	}
	if nativeTier(nativeCanaryHandler) != guard.BestEffort {
		t.Fatal("dedicated handler was not best-effort")
	}
	if err := nativeBinding("kata", nativeKataRuntime, "shim"); err == nil {
		t.Fatal("remote/shim sandboxer bypassed private parent placement")
	}
	if err := nativeBinding(nativeCanaryHandler, "io.containerd.runc.v2", nativePodSandboxMode); err == nil {
		t.Fatal("best-effort handler accepted a non-Kata runtime")
	}
	if err := nativeBinding(nativeCanaryHandler, nativeKataRuntime, nativePodSandboxMode); err != nil {
		t.Fatal(err)
	}
	if err := nativeBinding("runc", nativeRuncRuntime, nativePodSandboxMode); err != nil {
		t.Fatalf("stock runc protected handler refused: %v", err)
	}
	for _, runtimeType := range []string{"io.containerd.runsc.v1", "io.containerd.kata-qemu.v2", "io.containerd.custom.v2", ""} {
		if err := nativeBinding("custom", runtimeType, nativePodSandboxMode); err == nil {
			t.Fatalf("unverified runtime %q could ignore the private parent", runtimeType)
		}
	}
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	if err := h.memory.create(context.Background(), "s", "gvisor", "io.containerd.runsc.v1", nativePodSandboxMode, nativePod("/kubepods/pods", 1024, 0)); err == nil {
		t.Fatal("unsupported runtime admitted")
	}
	if _, ok := h.driver.Contract("s"); ok {
		t.Fatal("unsupported runtime reached Guard")
	}
}

func TestNativeCreateRedirectsOnlyRuntimeCopyIntoPrivateChild(t *testing.T) {
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	ctx := context.Background()
	protected := nativePod("/kubepods/burstable/poda", 1024, 256)
	if err := h.memory.create(ctx, "a", "kata", nativeKataRuntime, nativePodSandboxMode, protected); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("a"))
	if want := "/kubepods/burstable/poda/native-" + hex.EncodeToString(sum[:]); protected.Linux.CgroupParent != want {
		t.Fatalf("protected parent %q, want %q", protected.Linux.CgroupParent, want)
	}
	be := nativePod("/kubepods/besteffort/podb", 1024, 0)
	if err := h.memory.create(ctx, "b", nativeCanaryHandler, nativeKataRuntime, nativePodSandboxMode, be); err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256([]byte("b"))
	if want := "/sylphx-besteffort/pod-" + hex.EncodeToString(sum[:]); be.Linux.CgroupParent != want {
		t.Fatalf("best-effort parent %q, want %q", be.Linux.CgroupParent, want)
	}
	for _, parent := range []string{"/system/pod", "/kubepods/../system", "/sylphx-besteffort/pod-x", "/kubepods/burstable/poda/native-x", "kubepods.slice:cri:x"} {
		config := nativePod(parent, 1024, 0)
		if err := h.memory.create(ctx, "x", "kata", nativeKataRuntime, nativePodSandboxMode, config); err == nil {
			t.Fatalf("caller-selected parent %q admitted", parent)
		}
	}
	if _, ok := h.driver.Contract("x"); ok {
		t.Fatal("refused parent reached Guard")
	}
}

func TestNativeRefusedAdmissionLeavesNoIntentOrOrphan(t *testing.T) {
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	h.driver.deny = fmt.Errorf("candidate exceeds live room: %w", guard.ErrNoRoom)
	config := nativePod("/kubepods/burstable/poda", 1024, 256)
	err := h.memory.create(context.Background(), "a", "kata", nativeKataRuntime, nativePodSandboxMode, config)
	if !errors.Is(err, guard.ErrNoRoom) {
		t.Fatalf("expected ErrNoRoom, got %v", err)
	}
	if config.Linux.CgroupParent != "/kubepods/burstable/poda" {
		t.Fatal("refused admission redirected the runtime config")
	}
	h.memory.discard(context.Background(), "a")
	if _, ok := h.driver.Contract("a"); ok || len(h.memory.orphans) != 0 {
		t.Fatal("refused admission left durable or retried intent")
	}
}

func TestNativeResizeUpDownUpUsesPodEnvelopeWithoutDeltaAccumulation(t *testing.T) {
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	ctx := context.Background()
	if err := h.memory.create(ctx, "a", "kata", nativeKataRuntime, nativePodSandboxMode, nativePod("/kubepods/burstable/poda", 1024, 256)); err != nil {
		t.Fatal(err)
	}
	// Kubelet growth order: pod cgroup, UpdatePodSandboxResources, container.
	if err := h.memory.podUpdate(ctx, "a", nativeLimits(1024), nativeLimits(256), nativeLimits(2048), nativeLimits(256)); err != nil {
		t.Fatal(err)
	}
	c, _ := h.driver.Contract("a")
	if err := nativeContainerFits(c.Envelope, 2048, 256, nil); err != nil {
		t.Fatalf("container growth inside admitted pod envelope refused: %v", err)
	}
	if err := nativeContainerFits(c.Envelope, 4096, 256, nil); err == nil {
		t.Fatal("container growth beyond the admitted pod envelope accepted")
	}
	if err := h.memory.podUpdate(ctx, "a", nativeLimits(2048), nativeLimits(256), nativeLimits(1024), nativeLimits(256)); err != nil {
		t.Fatal(err)
	}
	if err := h.memory.podUpdate(ctx, "a", nativeLimits(1024), nativeLimits(256), nativeLimits(2048), nativeLimits(256)); err != nil {
		t.Fatal(err)
	}
	want := []uint64{1280, 2304, 1280, 2304}
	if fmt.Sprint(h.driver.reserves) != fmt.Sprint(want) {
		t.Fatalf("reserved envelopes %v, want %v (no repeated deltas)", h.driver.reserves, want)
	}
	if err := nativeContainerFits(2304, 1024, 256, []int64{1024, 0}); err == nil {
		t.Fatal("unlimited sibling admitted inside finite envelope")
	}
	if err := nativeContainerFits(2304, 1024, 256, []int64{1024}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCPUOnlyUpdatesNeverReachGuard(t *testing.T) {
	h := newNativeHarness(t)
	h.openErr = errors.New("guard unavailable")
	h.memory.adopt(context.Background())
	if err := h.memory.podUpdate(context.Background(), "legacy", nativeLimits(0), nil, &runtime.LinuxContainerResources{CpuShares: 512}, nil); err != nil {
		t.Fatalf("CPU-only pod update on unlimited context rejected: %v", err)
	}
	unlimited := &runtimespec.Spec{Linux: &runtimespec.Linux{Resources: &runtimespec.LinuxResources{}}}
	if err := (&criService{}).nativeContainerResize(context.Background(), containerstore.Container{}, unlimited, unlimited); err != nil {
		t.Fatalf("CPU-only container update on unlimited context rejected: %v", err)
	}
	limit := int64(1024)
	limited := &runtimespec.Spec{Linux: &runtimespec.Linux{Resources: &runtimespec.LinuxResources{Memory: &runtimespec.LinuxMemory{Limit: &limit}}}}
	if err := (&criService{}).nativeContainerResize(context.Background(), containerstore.Container{}, limited, limited); err != nil {
		t.Fatalf("unchanged memory limit reached memory error path: %v", err)
	}
	if err := h.memory.podUpdate(context.Background(), "legacy", nativeLimits(0), nil, nativeLimits(1024), nil); err == nil {
		t.Fatal("memory growth without an open guard accepted")
	}
}

func TestNativeRecoveryReconstructsFromGuardAndMetadata(t *testing.T) {
	h := newNativeHarness(t)
	ctx := context.Background()
	parent := func(id string) string { return nativePrivateParent(id, guard.Protected, "/kubepods/pod"+id) }
	for _, id := range []string{"owned", "released", "orphan", "tombstone", "held"} {
		if _, err := h.driver.Reserve(ctx, id, guard.Protected, 1024, 1024, "/kubepods/pod"+id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"released", "tombstone"} {
		if err := h.driver.Release(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	h.driver.held["held"] = true
	h.sandboxes = []nativeSandbox{
		{ID: "owned", Handler: "kata", Parent: parent("owned"), Live: true},
		{ID: "released", Handler: "kata", Parent: parent("released")},
		{ID: "legacy-stopped", Handler: "runc", Parent: "/kubepods/podlegacy"},
		{ID: "legacy-live", Handler: "runc", Parent: "/kubepods/podlive", Live: true},
	}
	h.live["owned"], h.live["legacy-live"] = true, true
	h.memory.adopt(ctx)
	for _, id := range []string{"orphan", "tombstone"} {
		if _, ok := h.driver.Contract(id); ok {
			t.Fatalf("contract %s without metadata was not released and forgotten", id)
		}
	}
	if c, _ := h.driver.Contract("released"); c.State != guard.StateReleased {
		t.Fatal("released tombstone with metadata was forgotten before metadata deletion")
	}
	if !h.memory.orphans["held"] {
		t.Fatal("held orphan was not retained for retry")
	}
	err := h.memory.create(ctx, "new", "kata", nativeKataRuntime, nativePodSandboxMode, nativePod("/kubepods/podnew", 1024, 0))
	if err == nil {
		t.Fatal("unowned live legacy sandbox did not close native allocation")
	}
	if _, ok := h.driver.Contract("new"); ok {
		t.Fatal("closed allocation reached Guard")
	}
	// Stopping the legacy sandbox reopens allocation; stopped legacy state is adopted.
	h.live["legacy-live"] = false
	if err := h.memory.create(ctx, "new", "kata", nativeKataRuntime, nativePodSandboxMode, nativePod("/kubepods/podnew", 1024, 0)); err != nil {
		t.Fatal(err)
	}
	if err := h.memory.release(ctx, "legacy-stopped"); err != nil {
		t.Fatalf("stopped legacy sandbox removal blocked: %v", err)
	}
	// The held orphan settles on the next removal once it empties.
	h.driver.held["held"] = false
	h.memory.forget(ctx, "legacy-stopped")
	if _, ok := h.driver.Contract("held"); ok || h.memory.orphans["held"] {
		t.Fatal("held orphan never settled")
	}
}

func TestNativeRemovalKeepsTombstoneUntilMetadataDeleted(t *testing.T) {
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	ctx := context.Background()
	if err := h.memory.create(ctx, "a", "kata", nativeKataRuntime, nativePodSandboxMode, nativePod("/kubepods/poda", 1024, 0)); err != nil {
		t.Fatal(err)
	}
	h.driver.held["a"] = true
	if err := h.memory.release(ctx, "a"); !errors.Is(err, guard.ErrHeld) {
		t.Fatalf("populated sandbox release must be retryable held, got %v", err)
	}
	if c, _ := h.driver.Contract("a"); c.State != guard.StateAdmitted {
		t.Fatal("held release lost conservative ownership")
	}
	h.driver.held["a"] = false
	if err := h.memory.release(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := h.memory.fence(ctx, "a"); err == nil {
		t.Fatal("released tombstone admitted a new allocation")
	}
	// Crash here: metadata still exists. Recovery keeps the tombstone.
	h.sandboxes = []nativeSandbox{{ID: "a", Handler: "kata", Parent: nativePrivateParent("a", guard.Protected, "/kubepods/poda")}}
	restarted := newNativeHarness(t)
	restarted.driver, restarted.sandboxes = h.driver, h.sandboxes
	restarted.memory.adopt(ctx)
	if c, ok := h.driver.Contract("a"); !ok || c.State != guard.StateReleased {
		t.Fatal("recovery dropped released tombstone while metadata exists")
	}
	if err := restarted.memory.release(ctx, "a"); err != nil {
		t.Fatalf("retried removal of released sandbox failed: %v", err)
	}
	restarted.memory.forget(ctx, "a")
	if _, ok := h.driver.Contract("a"); ok {
		t.Fatal("tombstone survived metadata deletion")
	}
}

func TestNativeOpenFailureClosesAllocationAndRetries(t *testing.T) {
	h := newNativeHarness(t)
	h.openErr = errors.New("guard state unavailable")
	h.memory.adopt(context.Background())
	config := nativePod("/kubepods/poda", 1024, 0)
	if err := h.memory.create(context.Background(), "a", "kata", nativeKataRuntime, nativePodSandboxMode, config); err == nil {
		t.Fatal("allocation proceeded without Guard")
	}
	h.openErr = nil
	if err := h.memory.create(context.Background(), "a", "kata", nativeKataRuntime, nativePodSandboxMode, config); err != nil {
		t.Fatal(err)
	}
	if err := h.memory.close(); err != nil {
		t.Fatal(err)
	}
	if err := h.memory.fence(context.Background(), "a"); err == nil {
		t.Fatal("closed runtime reopened Guard")
	}
}

func TestNativeContainerConfigMapsOriginalOrPrivateOnly(t *testing.T) {
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	ctx := context.Background()
	if err := h.memory.create(ctx, "a", "runc", "io.containerd.runc.v2", nativePodSandboxMode, nativePod("/kubepods/poda", 1024, 0)); err != nil {
		t.Fatal(err)
	}
	private := nativePrivateParent("a", guard.Protected, "/kubepods/poda")
	for _, parent := range []string{"/kubepods/poda", private} {
		original := nativePod(parent, 1024, 0)
		effective, err := h.memory.containerConfig(ctx, "a", "runc", original)
		if err != nil {
			t.Fatal(err)
		}
		if effective.Linux.CgroupParent != private || original.Linux.CgroupParent != parent {
			t.Fatalf("parent %q mapped to %q (request mutated to %q)", parent, effective.Linux.CgroupParent, original.Linux.CgroupParent)
		}
	}
	if _, err := h.memory.containerConfig(ctx, "a", "runc", nativePod("/kubepods/podother", 1024, 0)); err == nil {
		t.Fatal("changed parent identity accepted")
	}
	if _, err := h.memory.containerConfig(ctx, "a", nativeCanaryHandler, nativePod("/kubepods/poda", 1024, 0)); err == nil {
		t.Fatal("changed handler tier accepted")
	}
}

func TestNativeConcurrentCreatesKeepEveryContract(t *testing.T) {
	h := newNativeHarness(t)
	h.memory.adopt(context.Background())
	var group sync.WaitGroup
	for i := range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			id := fmt.Sprint(i)
			if err := h.memory.create(context.Background(), id, "kata", nativeKataRuntime, nativePodSandboxMode, nativePod("/kubepods/pod"+id, 1024, 0)); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if len(h.driver.Contracts()) != 16 {
		t.Fatal("concurrent create lost ownership")
	}
}

// Budget tests use a file-backed cgroup tree with the kernel's file names.

type nativeBudgetFixture struct {
	dir    string
	reader *nativeBudgetReader
}

const talosKubelet = "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncgroupRoot: /\nsystemReserved:\n  cpu: 50m\n  memory: 384Mi\n"

func newNativeBudgetFixture(t *testing.T, kubelet string) *nativeBudgetFixture {
	t.Helper()
	dir := t.TempDir()
	f := &nativeBudgetFixture{dir: dir, reader: &nativeBudgetReader{configPath: filepath.Join(dir, "native.json"), meminfoPath: filepath.Join(dir, "meminfo"), cgroupRoot: filepath.Join(dir, "cgroup")}}
	f.kubelet(t, kubelet)
	f.root(t, 0, 0, 0)
	return f
}

func (f *nativeBudgetFixture) kubelet(t *testing.T, kubelet string) {
	t.Helper()
	path := filepath.Join(f.dir, "kubelet.yaml")
	nativeWrite(t, path, kubelet)
	sum := sha256.Sum256([]byte(kubelet))
	nativeWrite(t, f.reader.configPath, fmt.Sprintf(`{"version":1,"nodeIdentity":"worker","kubeletConfigPath":%q,"kubeletConfigSHA256":%q,"podCgroupParents":["/sylphx-besteffort","/kubepods"]}`, path, hex.EncodeToString(sum[:])))
}

func (f *nativeBudgetFixture) cgroup(t *testing.T, name string, current, activeFile, inactiveFile, dirty, writeback uint64) {
	t.Helper()
	dir := filepath.Join(f.reader.cgroupRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	nativeWrite(t, filepath.Join(dir, "memory.current"), fmt.Sprintf("%d\n", current))
	nativeWrite(t, filepath.Join(dir, "memory.stat"), fmt.Sprintf("anon 1\nfile 9\nactive_file %d\ninactive_file %d\nfile_dirty %d\nfile_writeback %d\nshmem 4096\n", activeFile, inactiveFile, dirty, writeback))
}

// root writes the root cgroup memory.stat that kubelet's node working set uses.
func (f *nativeBudgetFixture) root(t *testing.T, anon, file, inactiveFile uint64) {
	t.Helper()
	if err := os.MkdirAll(f.reader.cgroupRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	nativeWrite(t, filepath.Join(f.reader.cgroupRoot, "memory.stat"), fmt.Sprintf("anon %d\nfile %d\nkernel 7\nactive_file 1\ninactive_file %d\n", anon, file, inactiveFile))
}

func nativeWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

const mib = uint64(1 << 20)

func TestNativeBudgetTalosOmittedFieldsUsePinnedKubeletDefaults(t *testing.T) {
	f := newNativeBudgetFixture(t, talosKubelet)
	nativeWrite(t, f.reader.meminfoPath, fmt.Sprintf("MemTotal: %d kB\nMemFree: %d kB\nMemAvailable: %d kB\n", 8192*1024, 2048*1024, 5000*1024))
	// Root children: kubepods (protected original + dying unreserved residue),
	// the BE aggregate, and a system child holding clean and dirty page cache.
	f.cgroup(t, "kubepods", 2600*mib, 300*mib, 200*mib, 0, 0)
	f.cgroup(t, "kubepods/burstable/poda", 1500*mib, 100*mib, 0, 0, 0)
	f.cgroup(t, "sylphx-besteffort", 1000*mib, 50*mib, 50*mib, 0, 0)
	f.cgroup(t, "system", 900*mib, 400*mib, 200*mib, 50*mib, 25*mib)
	// Kubelet node available = 8192 - (anon 2000 + file 3000 - inactive 1000)
	// = 4192Mi, below MemAvailable 5000Mi: the smaller signal is supplied.
	f.root(t, 2000*mib, 3000*mib, 1000*mib)
	budget, err := f.reader.read([]string{"/kubepods/burstable/poda"})
	if err != nil {
		t.Fatal(err)
	}
	// occupied 6144Mi; pods = BE 1000 + reserved original 1500 = 2500Mi;
	// root clean = 500 + 100 + 525 = 1125Mi, pod clean = 100 + 100 => 925Mi
	// non-pod clean. Essential = 6144 - 2500 - 925 = 2719Mi, which retains the
	// 1100Mi unreserved/dying kubepods residue and dirty/writeback pages.
	want := guard.Budget{Physical: 8192 * mib, Allocatable: (8192 - 384 - 100) * mib, Essential: 2719 * mib, Eviction: 100 * mib,
		Available: 4192 * mib, ProtectedResident: 1500 * mib, BestEffortResident: 1000 * mib}
	if budget != want {
		t.Fatalf("budget %+v, want %+v", budget, want)
	}
	// An original kubelet already removed is proof it emptied: zero, not error.
	if budget, err = f.reader.read([]string{"/kubepods/burstable/podgone"}); err != nil || budget.ProtectedResident != 0 {
		t.Fatalf("removed original: %+v %v", budget, err)
	}
	// Kubelet's signal above MemAvailable: MemAvailable is the bound.
	f.root(t, 1000*mib, 1000*mib, 500*mib)
	if budget, err = f.reader.read(nil); err != nil || budget.Available != 5000*mib {
		t.Fatalf("MemAvailable bound: %+v %v", budget, err)
	}
	if err := os.Remove(filepath.Join(f.reader.cgroupRoot, "memory.stat")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reader.read(nil); err == nil {
		t.Fatal("budget produced without kubelet's node availability signal")
	}
}

func TestNativeBudgetEvictionSemanticsMatchKubelet(t *testing.T) {
	f := newNativeBudgetFixture(t, talosKubelet+"evictionHard:\n  nodefs.available: 10%\nevictionSoft:\n  memory.available: 7.5%\n")
	nativeWrite(t, f.reader.meminfoPath, "MemTotal: 10485760 kB\nMemFree: 9437184 kB\nMemAvailable: 9437184 kB\n")
	f.cgroup(t, "sylphx-besteffort", 0, 0, 0, 0, 0)
	budget, err := f.reader.read(nil)
	if err != nil {
		t.Fatal(err)
	}
	physical := uint64(10 << 30)
	soft := uint64(int64(float64(int64(physical)) * float64(float32(7.5)/100)))
	// Explicit evictionHard without memory.available and no merge: no memory
	// hard threshold; soft affects only Eviction, never allocatable.
	if budget.Eviction != soft || budget.Allocatable != physical-384*mib {
		t.Fatalf("eviction=%d allocatable=%d, want %d %d", budget.Eviction, budget.Allocatable, soft, physical-384*mib)
	}
	// Observed essential (1Gi) exceeds the configured host reservation.
	if budget.Essential != 1024*mib {
		t.Fatalf("essential=%d", budget.Essential)
	}
	// Idle host: H is the configured kube+system reservation, not observation.
	nativeWrite(t, f.reader.meminfoPath, "MemTotal: 10485760 kB\nMemFree: 10383360 kB\nMemAvailable: 10383360 kB\n")
	if budget, err = f.reader.read(nil); err != nil || budget.Essential != 384*mib {
		t.Fatalf("host reservation floor: %+v %v", budget, err)
	}
	f.kubelet(t, talosKubelet+"mergeDefaultEvictionSettings: true\nevictionHard:\n  nodefs.available: 10%\n")
	if budget, err = f.reader.read(nil); err != nil || budget.Eviction != 100*mib || budget.Allocatable != physical-484*mib {
		t.Fatalf("merged default hard eviction: %+v %v", budget, err)
	}
	f.kubelet(t, talosKubelet+"evictionHard:\n  memory.available: 10%\n")
	tenth := uint64(int64(float64(int64(physical)) * float64(float32(10)/100)))
	if budget, err = f.reader.read(nil); err != nil || budget.Eviction != tenth || budget.Allocatable != physical-384*mib-tenth {
		t.Fatalf("percentage hard eviction: %+v %v", budget, err)
	}
}

func TestNativeBudgetRefusesUnsupportedOrChangedHierarchy(t *testing.T) {
	f := newNativeBudgetFixture(t, talosKubelet)
	nativeWrite(t, f.reader.meminfoPath, "MemTotal: 1048576 kB\nMemFree: 524288 kB\nMemAvailable: 524288 kB\n")
	f.cgroup(t, "sylphx-besteffort", 0, 0, 0, 0, 0)
	if _, err := f.reader.read([]string{"/system/pod"}); err == nil {
		t.Fatal("original outside kubelet pod root accepted")
	}
	if _, err := f.reader.read([]string{"/kubepods/burstable", "/kubepods/burstable/poda"}); err == nil {
		t.Fatal("overlapping originals accepted")
	}
	for _, kubelet := range []string{talosKubelet + "cgroupDriver: systemd\n", talosKubelet + "cgroupsPerQOS: false\n", "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\ncgroupRoot: /nested\n"} {
		f.kubelet(t, kubelet)
		if _, err := f.reader.read(nil); err == nil {
			t.Fatalf("unsupported kubelet hierarchy accepted:\n%s", kubelet)
		}
	}
	f.kubelet(t, talosKubelet)
	nativeWrite(t, filepath.Join(f.dir, "kubelet.yaml"), talosKubelet+"kubeReserved: {memory: 1Gi}\n")
	if _, err := f.reader.read(nil); err == nil {
		t.Fatal("changed effective kubelet config accepted without readback")
	}
	if err := os.Remove(f.reader.configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reader.read(nil); err == nil {
		t.Fatal("missing native config produced a budget")
	}
}

// Pressure watcher: event-driven reconcile and bounded shutdown.

func TestNativePressureWatcherReconcilesOnPriorityEventAndStops(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	file, err := server.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(int(file.Fd()))
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	fired := make(chan struct{})
	w, err := watchNativePressure(fd, func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(fired)
		}
		<-ctx.Done()
		return ctx.Err()
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
		t.Fatal("reconcile ran without a kernel priority event")
	case <-time.After(50 * time.Millisecond):
	}
	raw, err := client.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var sendErr error
	if err := raw.Control(func(fd uintptr) { sendErr = unix.Sendto(int(fd), []byte{1}, unix.MSG_OOB, nil) }); err != nil || sendErr != nil {
		t.Fatal(err, sendErr)
	}
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("priority event did not reconcile")
	}
	start := time.Now()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("shutdown did not cancel the in-flight reconcile")
	}
}

func TestNativePressureWatcherStopsWhenMonitorIsDestroyed(t *testing.T) {
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	w, err := watchNativePressure(pipe[0], func(context.Context) error {
		t.Error("hangup treated as pressure")
		return nil
	}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(pipe[1])
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("destroyed monitor left the watcher running")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
