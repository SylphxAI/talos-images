package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/SylphxAI/talos-images/host-memory/guard"
	"github.com/containerd/log"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Native host-memory integration for the CRI plugin. Guard is the single
// durable record of admission, intent and release; this adapter keeps only
// in-memory views derived from Guard contracts and containerd metadata.
const (
	nativeMemoryState    = "/var/lib/containerd/host-memory/guard"
	nativeMemoryConfig   = "/var/etc/sylphx-host-memory.json"
	nativeCgroupRoot     = "/sys/fs/cgroup"
	nativePodRoot        = "/kubepods"
	nativeAggregate      = "/sylphx-besteffort"
	nativeCanaryHandler  = "kata-memory-canary"
	nativeKataRuntime    = "io.containerd.kata.v2"
	nativeRuncRuntime    = "io.containerd.runc.v2"
	nativePodSandboxMode = "podsandbox"
	nativePressurePath   = "/proc/pressure/memory"
	nativeReconcileLimit = 5 * time.Second
)

// memoryDriver is the exact Guard surface the runtime calls. *guard.Guard
// implements it; tests substitute the contract state machine only.
type memoryDriver interface {
	Reserve(ctx context.Context, id string, tier guard.Tier, envelope, floor uint64, originalParent string) (guard.Contract, error)
	Fence(ctx context.Context, id string) (guard.Contract, error)
	Release(ctx context.Context, id string) error
	Forget(ctx context.Context, id string) error
	Contract(id string) (guard.Contract, bool)
	Contracts() []guard.Contract
	Reconcile(ctx context.Context) error
	Close() error
}

// nativeSandbox is containerd's recovered sandbox metadata, reduced to the
// fields that identify native memory ownership.
type nativeSandbox struct {
	ID      string
	Handler string
	Parent  string
	Live    bool
}

type nativeMemory struct {
	mu        sync.Mutex
	open      func() (memoryDriver, error)
	inventory func() []nativeSandbox
	live      func(id string) bool
	watch     func(func(context.Context) error) (interface{ Close() error }, error)
	driver    memoryDriver
	watcher   interface{ Close() error }
	recovered bool
	closed    bool
	// unowned live sandboxes predate native ownership (or disagree with it).
	// Their unbounded growth invalidates every guarantee, so native allocation
	// stays closed until each one is no longer live.
	unowned map[string]string
	// orphans are contracts without containerd metadata whose release is held.
	orphans map[string]bool
}

func newNativeMemory(open func() (memoryDriver, error), inventory func() []nativeSandbox, live func(string) bool,
	watch func(func(context.Context) error) (interface{ Close() error }, error)) *nativeMemory {
	return &nativeMemory{open: open, inventory: inventory, live: live, watch: watch, unowned: map[string]string{}, orphans: map[string]bool{}}
}

func openNativeGuard() (memoryDriver, error) {
	budget := &nativeBudgetReader{configPath: nativeMemoryConfig, meminfoPath: "/proc/meminfo", cgroupRoot: nativeCgroupRoot}
	// Guard reads the kernel boot_id itself; prior-boot contracts become
	// released tombstones, never re-admitted demand.
	return guard.Open(guard.Config{CgroupRoot: nativeCgroupRoot, PodRoot: nativePodRoot, StateDir: nativeMemoryState, NodeBudget: budget.read})
}

func nativeTier(handler string) guard.Tier {
	if handler == nativeCanaryHandler {
		return guard.BestEffort
	}
	return guard.Protected
}

func nativeDigest(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// nativePrivateParent is the runtime-owned per-sandbox child for both tiers.
func nativePrivateParent(id string, tier guard.Tier, original string) string {
	if tier == guard.BestEffort {
		return nativeAggregate + "/pod-" + nativeDigest(id)
	}
	return original + "/native-" + nativeDigest(id)
}

// nativeOriginal validates the kubelet-supplied pod parent: a clean cgroupfs
// path strictly below the kubelet pod root, never the runtime-owned hierarchy.
func nativeOriginal(parent string) (string, error) {
	if parent == "" || strings.ContainsAny(parent, ":\x00") || !path.IsAbs(parent) || path.Clean(parent) != parent {
		return "", fmt.Errorf("clean absolute cgroupfs pod parent required, got %q", parent)
	}
	if !strings.HasPrefix(parent, nativePodRoot+"/") {
		return "", fmt.Errorf("pod parent %q is outside kubelet pod root %s", parent, nativePodRoot)
	}
	if base := path.Base(parent); strings.HasPrefix(base, "native-") {
		return "", fmt.Errorf("CRI caller cannot select a runtime-owned memory child")
	}
	return parent, nil
}

// nativeEnvelope uses kubelet's effective pod contract, which already combines
// regular, init and restartable-init containers. Overhead is added once. A
// zero limit means kubelet found at least one unlimited container or no limit;
// such pods are outside the finite protected envelope precondition.
func nativeEnvelope(resources, overhead *runtime.LinuxContainerResources) (uint64, uint64, error) {
	if resources.GetMemoryLimitInBytes() <= 0 {
		return 0, 0, errors.New("finite effective pod memory limit is required on native host-memory nodes")
	}
	if overhead.GetMemoryLimitInBytes() < 0 {
		return 0, 0, errors.New("negative pod memory overhead")
	}
	limit, extra := uint64(resources.GetMemoryLimitInBytes()), uint64(overhead.GetMemoryLimitInBytes())
	if limit > math.MaxInt64-extra {
		return 0, 0, errors.New("pod memory envelope overflow")
	}
	// CRI has no pod memory request. Full requests remain in kubelet, so the
	// finite envelope is also the reclaim floor rather than an invented request.
	return limit + extra, limit + extra, nil
}

// nativeBinding accepts only runtimes whose sandbox and every later cgroup are
// created by the in-process podsandbox controller inside the private parent:
// stock runc, or the native Kata shim (which itself refuses non-local-CLH,
// factory, jailer, remote or restore paths). Any other runtime or sandboxer
// could ignore CgroupParent and escape host accounting, so it is refused
// before a shim exists. Only native Kata may claim the best-effort tier.
func nativeBinding(handler, runtimeType, sandboxer string) error {
	if sandboxer != nativePodSandboxMode {
		return fmt.Errorf("runtime handler %q must use the %s sandboxer, got %q", handler, nativePodSandboxMode, sandboxer)
	}
	switch {
	case runtimeType == nativeKataRuntime:
	case runtimeType == nativeRuncRuntime && handler != nativeCanaryHandler:
	default:
		return fmt.Errorf("runtime handler %q type %q is not a supported native host-memory runtime", handler, runtimeType)
	}
	return nil
}

// ensureLocked opens Guard lazily and reconstructs adapter views. An open
// failure is retried on the next allocation; it never stops image or status
// APIs, and allocation stays closed meanwhile.
func (m *nativeMemory) ensureLocked(ctx context.Context) error {
	if m.driver != nil {
		return nil
	}
	if m.closed {
		return errors.New("native host memory guard closed")
	}
	if !m.recovered {
		return errors.New("native host memory recovery has not completed")
	}
	driver, err := m.open()
	if err != nil {
		return fmt.Errorf("open native host memory guard: %w", err)
	}
	m.driver = driver
	m.adoptLocked(ctx)
	if m.watch != nil {
		watcher, err := m.watch(driver.Reconcile)
		if err != nil {
			// PSI only supplements the aggregate hard bound and fences.
			log.G(ctx).WithError(err).Warn("native host memory pressure watcher unavailable")
		} else {
			m.watcher = watcher
		}
	}
	return nil
}

// adopt runs once containerd metadata is recovered.
func (m *nativeMemory) adopt(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recovered = true
	if err := m.ensureLocked(ctx); err != nil {
		log.G(ctx).WithError(err).Error("native host memory unavailable; sandbox allocation closed")
	}
}

// adoptLocked classifies Guard contracts against containerd metadata:
//   - metadata without contract: stopped legacy state is adopted (its charges
//     count as host use in the live budget); live legacy state closes allocation;
//   - released contract with metadata: removal in progress, Forget after delete;
//   - contract without metadata: release then Forget; held releases are retried.
func (m *nativeMemory) adoptLocked(ctx context.Context) {
	contracts := map[string]guard.Contract{}
	for _, c := range m.driver.Contracts() {
		contracts[c.ID] = c
	}
	known := map[string]bool{}
	for _, sb := range m.inventory() {
		known[sb.ID] = true
		c, ok := contracts[sb.ID]
		switch {
		case !ok:
			if sb.Live {
				m.unowned[sb.ID] = "live sandbox predates native host-memory ownership"
			}
		case c.State == guard.StateReleased:
		case c.Tier != nativeTier(sb.Handler) || c.Parent != sb.Parent:
			if sb.Live {
				m.unowned[sb.ID] = "live sandbox disagrees with its native host-memory contract"
			}
		}
	}
	for id := range contracts {
		if !known[id] {
			m.orphans[id] = true
		}
	}
	m.settleLocked(ctx)
}

// settleLocked releases and forgets contracts that no metadata references.
func (m *nativeMemory) settleLocked(ctx context.Context) {
	for id := range m.orphans {
		if err := m.driver.Release(ctx, id); err != nil {
			log.G(ctx).WithError(err).WithField("sandbox", id).Warn("native host memory orphan release held")
			continue
		}
		if err := m.driver.Forget(ctx, id); err != nil {
			log.G(ctx).WithError(err).WithField("sandbox", id).Warn("native host memory orphan tombstone retained")
			continue
		}
		delete(m.orphans, id)
	}
}

func (m *nativeMemory) gateLocked() error {
	for id, reason := range m.unowned {
		if m.live(id) {
			return fmt.Errorf("native allocation closed: sandbox %s: %s; stop or recreate it first", id, reason)
		}
		delete(m.unowned, id)
	}
	return nil
}

// allocation returns Guard only when allocation is open.
func (m *nativeMemory) allocation(ctx context.Context) (memoryDriver, error) {
	if m == nil {
		return nil, errors.New("native host memory guard not initialized")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLocked(ctx); err != nil {
		return nil, err
	}
	if err := m.gateLocked(); err != nil {
		return nil, err
	}
	return m.driver, nil
}

func (m *nativeMemory) current(ctx context.Context) (memoryDriver, error) {
	if m == nil {
		return nil, errors.New("native host memory guard not initialized")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureLocked(ctx); err != nil {
		return nil, err
	}
	return m.driver, nil
}

// create admits the sandbox and redirects only the runtime-owned copy of its
// config into the private child. The caller registers discard before calling.
func (m *nativeMemory) create(ctx context.Context, id, handler, runtimeType, sandboxer string, config *runtime.PodSandboxConfig) error {
	if err := nativeBinding(handler, runtimeType, sandboxer); err != nil {
		return err
	}
	linux := config.GetLinux()
	if linux == nil {
		return errors.New("Linux pod sandbox config is required")
	}
	original, err := nativeOriginal(linux.GetCgroupParent())
	if err != nil {
		return err
	}
	envelope, floor, err := nativeEnvelope(linux.GetResources(), linux.GetOverhead())
	if err != nil {
		return err
	}
	driver, err := m.allocation(ctx)
	if err != nil {
		return err
	}
	tier := nativeTier(handler)
	contract, err := driver.Reserve(ctx, id, tier, envelope, floor, original)
	if err != nil {
		return err
	}
	if contract.State != guard.StateAdmitted || contract.Tier != tier || contract.Original != original || contract.Parent != nativePrivateParent(id, tier, original) {
		return fmt.Errorf("native guard returned unexpected contract for sandbox %s", id)
	}
	linux.CgroupParent = contract.Parent
	return nil
}

// discard rolls back a failed RunPodSandbox once no metadata references it.
func (m *nativeMemory) discard(ctx context.Context, id string) {
	driver, err := m.current(ctx)
	if err != nil {
		log.G(ctx).WithError(err).WithField("sandbox", id).Error("native host memory rollback deferred")
		return
	}
	if err = driver.Release(ctx, id); err == nil {
		err = driver.Forget(ctx, id)
	}
	if err != nil {
		log.G(ctx).WithError(err).WithField("sandbox", id).Warn("native host memory rollback held")
		m.mu.Lock()
		m.orphans[id] = true
		m.mu.Unlock()
	}
}

// release must succeed before containerd metadata is deleted. Guard leaves a
// durable released tombstone; forget removes it only after metadata deletion.
func (m *nativeMemory) release(ctx context.Context, id string) error {
	driver, err := m.current(ctx)
	if err != nil {
		return err
	}
	if err := driver.Release(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.unowned, id)
	m.mu.Unlock()
	return nil
}

func (m *nativeMemory) forget(ctx context.Context, id string) {
	driver, err := m.current(ctx)
	if err == nil {
		err = driver.Forget(ctx, id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// Restart recovery forgets released tombstones without metadata.
		log.G(ctx).WithError(err).WithField("sandbox", id).Warn("native host memory tombstone retained")
		m.orphans[id] = true
		return
	}
	if m.driver != nil {
		m.settleLocked(ctx)
	}
}

// fence must succeed immediately before each allocation-capable call.
func (m *nativeMemory) fence(ctx context.Context, id string) error {
	driver, err := m.allocation(ctx)
	if err != nil {
		return err
	}
	contract, err := driver.Fence(ctx, id)
	if err != nil {
		return err
	}
	if contract.State != guard.StateAdmitted {
		return fmt.Errorf("sandbox %s native memory contract is %s", id, contract.State)
	}
	return nil
}

func (m *nativeMemory) admitted(ctx context.Context, id string) (guard.Contract, error) {
	driver, err := m.current(ctx)
	if err != nil {
		return guard.Contract{}, err
	}
	contract, ok := driver.Contract(id)
	if !ok || contract.State != guard.StateAdmitted {
		return guard.Contract{}, fmt.Errorf("sandbox %s lacks an admitted native memory contract", id)
	}
	return contract, nil
}

// containerConfig returns the private runtime copy used for container cgroup
// placement. Kubelet sends its original parent; recovered metadata may carry
// the private parent. Anything else is an identity change.
func (m *nativeMemory) containerConfig(ctx context.Context, id, handler string, config *runtime.PodSandboxConfig) (*runtime.PodSandboxConfig, error) {
	if config.GetLinux() == nil {
		return nil, errors.New("native Linux sandbox contract is required")
	}
	contract, err := m.admitted(ctx, id)
	if err != nil {
		return nil, err
	}
	if contract.Tier != nativeTier(handler) {
		return nil, fmt.Errorf("sandbox %s runtime handler changed native memory tier", id)
	}
	if parent := config.GetLinux().GetCgroupParent(); parent != contract.Original && parent != contract.Parent {
		return nil, fmt.Errorf("sandbox %s parent identity changed", id)
	}
	effective := *config
	linux := *config.Linux
	linux.CgroupParent = contract.Parent
	effective.Linux = &linux
	return &effective, nil
}

// podUpdate makes kubelet's pod-level update the authoritative envelope. An
// update that leaves memory unchanged (CPU-only resize) never touches Guard.
func (m *nativeMemory) podUpdate(ctx context.Context, id string, oldResources, oldOverhead, resources, overhead *runtime.LinuxContainerResources) error {
	if oldResources.GetMemoryLimitInBytes() == resources.GetMemoryLimitInBytes() && oldOverhead.GetMemoryLimitInBytes() == overhead.GetMemoryLimitInBytes() {
		return nil
	}
	envelope, floor, err := nativeEnvelope(resources, overhead)
	if err != nil {
		return err
	}
	driver, err := m.allocation(ctx)
	if err != nil {
		return err
	}
	contract, ok := driver.Contract(id)
	if !ok || contract.State != guard.StateAdmitted {
		return fmt.Errorf("sandbox %s lacks an admitted native memory contract; memory resize refused", id)
	}
	_, err = driver.Reserve(ctx, id, contract.Tier, envelope, floor, contract.Original)
	return err
}

// nativeContainerFits validates a container memory change inside the pod's
// admitted envelope. Pod updates already reserved the aggregate; container
// updates never add deltas. others are the limits of every other non-exited
// container in the sandbox.
func nativeContainerFits(envelope uint64, limit int64, overhead int64, others []int64) error {
	if limit <= 0 {
		return errors.New("finite container memory limit is required for native memory resize")
	}
	if overhead < 0 {
		return errors.New("negative pod memory overhead")
	}
	total := uint64(overhead)
	for _, value := range append([]int64{limit}, others...) {
		if value <= 0 {
			return errors.New("unlimited sibling container cannot share a finite native envelope")
		}
		if total > math.MaxInt64-uint64(value) {
			return errors.New("container memory sum overflow")
		}
		total += uint64(value)
	}
	if total > envelope {
		return fmt.Errorf("container memory %d exceeds admitted pod envelope %d; UpdatePodSandboxResources must admit growth first", total, envelope)
	}
	return nil
}

func (m *nativeMemory) close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	var err error
	if m.watcher != nil {
		err = m.watcher.Close()
		m.watcher = nil
	}
	if m.driver != nil {
		err = errors.Join(err, m.driver.Close())
		m.driver = nil
	}
	return err
}
