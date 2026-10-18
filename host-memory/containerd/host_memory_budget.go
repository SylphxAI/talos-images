package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/SylphxAI/talos-images/host-memory/guard"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// nativeBudgetConfig is rendered by Foundation from the effective kubelet file
// and host hierarchy readback. It is read on every admission so CRI image
// bootstrap never depends on it existing.
type nativeBudgetConfig struct {
	Version             int      `json:"version"`
	NodeIdentity        string   `json:"nodeIdentity"`
	KubeletConfigPath   string   `json:"kubeletConfigPath"`
	KubeletConfigSHA256 string   `json:"kubeletConfigSHA256"`
	PodCgroupParents    []string `json:"podCgroupParents"`
}

// nativeKubelet holds the KubeletConfiguration fields that determine pod
// hierarchy and node allocatable memory, before pinned defaulting.
type nativeKubelet struct {
	APIVersion                   string            `json:"apiVersion"`
	Kind                         string            `json:"kind"`
	CgroupRoot                   string            `json:"cgroupRoot"`
	CgroupsPerQOS                *bool             `json:"cgroupsPerQOS"`
	CgroupDriver                 string            `json:"cgroupDriver"`
	KubeReserved                 map[string]string `json:"kubeReserved"`
	SystemReserved               map[string]string `json:"systemReserved"`
	EvictionHard                 map[string]string `json:"evictionHard"`
	EvictionSoft                 map[string]string `json:"evictionSoft"`
	MergeDefaultEvictionSettings *bool             `json:"mergeDefaultEvictionSettings"`
}

// nativeKubeletDefaults applies Kubernetes v1.34.0 kubelet defaulting for an
// omitted field exactly; explicit values always win. Sources:
//   - pkg/kubelet/apis/config/v1beta1/defaults.go:166-171 (cgroupsPerQOS=true,
//     cgroupDriver=cgroupfs) and :239-241 (mergeDefaultEvictionSettings=false);
//   - cmd/kubelet/app/server.go:453-464 with pkg/kubelet/eviction/defaults_linux.go:23-24
//     (nil evictionHard becomes memory.available=100Mi; merge fills missing keys).
//
// Talos v1.14.1 kubelet_spec.go omits these fields and sets cgroupRoot "/".
func nativeKubeletDefaults(k *nativeKubelet) error {
	if k.APIVersion != "kubelet.config.k8s.io/v1beta1" || k.Kind != "KubeletConfiguration" {
		return fmt.Errorf("unsupported kubelet configuration %s/%s", k.APIVersion, k.Kind)
	}
	if k.CgroupsPerQOS == nil {
		value := true
		k.CgroupsPerQOS = &value
	}
	if k.CgroupDriver == "" {
		k.CgroupDriver = "cgroupfs"
	}
	merge := k.MergeDefaultEvictionSettings != nil && *k.MergeDefaultEvictionSettings
	if k.EvictionHard == nil {
		k.EvictionHard = map[string]string{"memory.available": "100Mi"}
	} else if _, ok := k.EvictionHard["memory.available"]; merge && !ok {
		k.EvictionHard["memory.available"] = "100Mi"
	}
	if !*k.CgroupsPerQOS || k.CgroupDriver != "cgroupfs" {
		return errors.New("native accounting requires kubelet cgroupfs driver with cgroupsPerQOS")
	}
	if k.CgroupRoot != "" && k.CgroupRoot != "/" {
		return fmt.Errorf("native accounting requires kubelet cgroupRoot \"/\" (pod root %s), got %q", nativePodRoot, k.CgroupRoot)
	}
	return nil
}

type nativeBudgetReader struct {
	configPath  string
	meminfoPath string
	cgroupRoot  string
}

func (r *nativeBudgetReader) load() (*nativeKubelet, error) {
	data, err := os.ReadFile(r.configPath)
	if err != nil {
		return nil, fmt.Errorf("read native memory config: %w", err)
	}
	var config nativeBudgetConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse native memory config: %w", err)
	}
	if config.Version != 1 || config.NodeIdentity == "" || !filepath.IsAbs(config.KubeletConfigPath) {
		return nil, errors.New("invalid native memory config identity/version")
	}
	roots := slices.Clone(config.PodCgroupParents)
	slices.Sort(roots)
	if !slices.Equal(roots, []string{nativePodRoot, nativeAggregate}) {
		return nil, fmt.Errorf("native memory config pod roots %v must be exactly %s and %s", config.PodCgroupParents, nativePodRoot, nativeAggregate)
	}
	kubeletData, err := os.ReadFile(config.KubeletConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read effective kubelet config: %w", err)
	}
	sum := sha256.Sum256(kubeletData)
	if hex.EncodeToString(sum[:]) != config.KubeletConfigSHA256 {
		return nil, errors.New("effective kubelet configuration changed; immutable readback required")
	}
	jsonData, err := yaml.YAMLToJSONStrict(kubeletData)
	if err != nil {
		return nil, err
	}
	var k nativeKubelet
	if err := json.Unmarshal(jsonData, &k); err != nil {
		return nil, err
	}
	if err := nativeKubeletDefaults(&k); err != nil {
		return nil, err
	}
	return &k, nil
}

func nativeQuantity(value string) (uint64, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil || q.Sign() < 0 {
		return 0, fmt.Errorf("invalid memory quantity %q", value)
	}
	v, ok := q.AsInt64()
	if !ok {
		return 0, fmt.Errorf("nonintegral/overflowing memory quantity %q", value)
	}
	return uint64(v), nil
}

// nativeThreshold reproduces kubelet's float32 percentage parsing and
// int64(float64(capacity)*float64(percentage)) conversion exactly
// (pkg/kubelet/eviction/helpers.go:427-433, api/types.go:111-117).
func nativeThreshold(value string, capacity uint64) (uint64, error) {
	if !strings.HasSuffix(value, "%") {
		return nativeQuantity(value)
	}
	parsed, err := strconv.ParseFloat(strings.TrimRight(value, "%"), 32)
	if err != nil || parsed < 0 || parsed > 100 {
		return 0, fmt.Errorf("invalid memory eviction percentage %q", value)
	}
	percentage := float32(parsed) / 100
	if capacity > math.MaxInt64 {
		return 0, errors.New("physical memory overflow")
	}
	return uint64(int64(float64(int64(capacity)) * float64(percentage))), nil
}

// nativeMeminfo is one bracketing observation. available is the smaller of
// the kernel's MemAvailable and kubelet's own node memory.available signal.
type nativeMeminfo struct{ total, free, available uint64 }

func (r *nativeBudgetReader) meminfo() (nativeMeminfo, error) {
	data, err := os.ReadFile(r.meminfoPath)
	if err != nil {
		return nativeMeminfo{}, err
	}
	fields := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 3 || (parts[0] != "MemTotal:" && parts[0] != "MemFree:" && parts[0] != "MemAvailable:") {
			continue
		}
		value, err := strconv.ParseUint(parts[1], 10, 64)
		if parts[2] != "kB" || err != nil || value > math.MaxUint64/1024 {
			return nativeMeminfo{}, fmt.Errorf("invalid %s observation", parts[0])
		}
		fields[parts[0]] = value * 1024
	}
	m := nativeMeminfo{total: fields["MemTotal:"], free: fields["MemFree:"], available: fields["MemAvailable:"]}
	if _, ok := fields["MemAvailable:"]; !ok || m.total == 0 || m.free > m.total || m.available > m.total {
		return nativeMeminfo{}, errors.New("missing or inconsistent physical memory observation")
	}
	kubelet, err := r.kubeletAvailable(m.total)
	if err != nil {
		return nativeMeminfo{}, err
	}
	m.available = min(m.available, kubelet)
	return m, nil
}

// kubeletAvailable reproduces kubelet v1.34's node memory.available: capacity
// (MemTotal) minus the root working set, where the cgroup-v2 root usage is
// anon+file from the root memory.stat (opencontainers/cgroups v0.0.1
// fs2/memory.go:234) and working set subtracts inactive_file (cadvisor v0.52.1
// container/libcontainer/handler.go:831-854). Kubelet evicts on this signal,
// which can exceed MemAvailable or fall below it.
func (r *nativeBudgetReader) kubeletAvailable(capacity uint64) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(r.cgroupRoot, "memory.stat"))
	if err != nil {
		return 0, fmt.Errorf("root memory.stat: %w", err)
	}
	stat := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 || (parts[0] != "anon" && parts[0] != "file" && parts[0] != "inactive_file") {
			continue
		}
		value, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid root memory.stat %s", parts[0])
		}
		stat[parts[0]] = value
	}
	if len(stat) != 3 {
		return 0, errors.New("root memory.stat lacks anon, file or inactive_file")
	}
	usage, err := nativeAdd(stat["anon"], stat["file"])
	if err != nil {
		return 0, err
	}
	return nativeSub(capacity, nativeSub(usage, stat["inactive_file"])), nil
}

// nativeCgroupMemory is one inclusive cgroup observation.
type nativeCgroupMemory struct{ current, clean uint64 }

func (r *nativeBudgetReader) cgroup(cgroup string, optional bool) (nativeCgroupMemory, error) {
	dir := filepath.Join(r.cgroupRoot, strings.TrimPrefix(cgroup, "/"))
	current, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if optional && errors.Is(err, os.ErrNotExist) {
		// A reserved original kubelet removed is kernel proof it emptied; its
		// reparented residue is measured in its ancestors as host use.
		return nativeCgroupMemory{}, nil
	}
	if err != nil {
		return nativeCgroupMemory{}, err
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(current)), 10, 64)
	if err != nil {
		return nativeCgroupMemory{}, fmt.Errorf("invalid %s/memory.current", cgroup)
	}
	clean, err := r.cleanFile(dir)
	if optional && errors.Is(err, os.ErrNotExist) {
		return nativeCgroupMemory{}, nil
	}
	return nativeCgroupMemory{current: value, clean: clean}, err
}

// cleanFile is reclaimable clean file LRU: active_file + inactive_file minus
// dirty and writeback pages. Shmem, anon and unevictable pages are retained.
func (r *nativeBudgetReader) cleanFile(dir string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(dir, "memory.stat"))
	if err != nil {
		return 0, err
	}
	stat := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "active_file", "inactive_file", "file_dirty", "file_writeback":
			value, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory.stat %s in %s", parts[0], dir)
			}
			stat[parts[0]] = value
		}
	}
	for _, key := range []string{"active_file", "inactive_file", "file_dirty", "file_writeback"} {
		if _, ok := stat[key]; !ok {
			return 0, fmt.Errorf("memory.stat in %s lacks %s", dir, key)
		}
	}
	lru, err := nativeAdd(stat["active_file"], stat["inactive_file"])
	if err != nil {
		return 0, err
	}
	busy, err := nativeAdd(stat["file_dirty"], stat["file_writeback"])
	if err != nil {
		return 0, err
	}
	return nativeSub(lru, busy), nil
}

// nativeSample is one snapshot of the disjoint direct root children and the
// accounted pod roots (BE aggregate and each reserved protected original once).
type nativeSample struct{ protected, observed, bestEffort, pods, nonPodClean uint64 }

func (r *nativeBudgetReader) sample(originals []guard.ProtectedOriginal) (nativeSample, error) {
	entries, err := os.ReadDir(r.cgroupRoot)
	if err != nil {
		return nativeSample{}, err
	}
	var rootClean uint64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		clean, err := r.cleanFile(filepath.Join(r.cgroupRoot, entry.Name()))
		if err != nil {
			return nativeSample{}, fmt.Errorf("exhaustive root child accounting: %w", err)
		}
		if rootClean, err = nativeAdd(rootClean, clean); err != nil {
			return nativeSample{}, err
		}
	}
	be, err := r.cgroup(nativeAggregate, false)
	if err != nil {
		return nativeSample{}, err
	}
	s := nativeSample{bestEffort: be.current}
	podClean := be.clean
	for _, original := range originals {
		o, err := r.cgroup(original.Path, true)
		if err != nil {
			return nativeSample{}, err
		}
		if original.Mode == guard.Observed {
			s.observed, err = nativeAdd(s.observed, o.current)
		} else {
			s.protected, err = nativeAdd(s.protected, o.current)
		}
		if err != nil {
			return nativeSample{}, err
		}
		if podClean, err = nativeAdd(podClean, o.clean); err != nil {
			return nativeSample{}, err
		}
	}
	accounted, err := nativeAdd(s.protected, s.observed)
	if err != nil {
		return nativeSample{}, err
	}
	if s.pods, err = nativeAdd(accounted, s.bestEffort); err != nil {
		return nativeSample{}, err
	}
	// Concurrent counter movement can make the pod subset briefly exceed the
	// root-child sum; clamping credits no reclaimable non-pod cache.
	s.nonPodClean = nativeSub(rootClean, podClean)
	return s, nil
}

func nativeOriginals(originals []guard.ProtectedOriginal) error {
	for i, original := range originals {
		if _, err := nativeOriginal(original.Path); err != nil {
			return err
		}
		if original.Mode != guard.Finite && original.Mode != guard.Observed {
			return errors.New("unknown protected accounting mode")
		}
		for _, other := range originals[:i] {
			if other.Path == original.Path || strings.HasPrefix(original.Path, other.Path+"/") || strings.HasPrefix(other.Path, original.Path+"/") {
				return fmt.Errorf("overlapping reserved protected originals %s and %s", other.Path, original.Path)
			}
		}
	}
	return nil
}

// read is Guard's live NodeBudget callback. originals are the deduplicated
// reserved protected kubelet pod parents. Only those and the BE aggregate are
// pod charges: dying or unreserved kubepods residue counts as host use.
func (r *nativeBudgetReader) read(originals []guard.ProtectedOriginal) (guard.Budget, error) {
	k, err := r.load()
	if err != nil {
		return guard.Budget{}, err
	}
	if err := nativeOriginals(originals); err != nil {
		return guard.Budget{}, err
	}
	before, err := r.meminfo()
	if err != nil {
		return guard.Budget{}, err
	}
	first, err := r.sample(originals)
	if err != nil {
		return guard.Budget{}, err
	}
	after, err := r.meminfo()
	if err != nil {
		return guard.Budget{}, err
	}
	if after.total != before.total {
		return guard.Budget{}, errors.New("physical memory changed during accounting")
	}
	second, err := r.sample(originals)
	if err != nil {
		return guard.Budget{}, err
	}
	physical := before.total
	// Max occupied bytes minus min accounted pod charges minus min reclaimable
	// non-pod clean file cache. Unaccounted kernel memory, dirty/writeback,
	// shmem and unreserved/dying pod residue all stay essential.
	occupied := max(before.total-before.free, after.total-after.free)
	deduction, err := nativeAdd(min(first.pods, second.pods), min(first.nonPodClean, second.nonPodClean))
	if err != nil {
		return guard.Budget{}, err
	}
	if deduction > occupied {
		return guard.Budget{}, errors.New("inconsistent physical/pod memory observations")
	}
	hard, err := nativeEvictionThreshold(k.EvictionHard, physical)
	if err != nil {
		return guard.Budget{}, err
	}
	soft, err := nativeEvictionThreshold(k.EvictionSoft, physical)
	if err != nil {
		return guard.Budget{}, err
	}
	// Kubelet allocatable = capacity - kubeReserved - systemReserved - hard
	// eviction (cmd/kubelet/app/server.go:805-822). Soft eviction affects only
	// the physical bound, through Eviction, never allocatable.
	var hostReserved uint64
	for _, values := range []map[string]string{k.KubeReserved, k.SystemReserved} {
		value, ok := values["memory"]
		if !ok {
			continue
		}
		bytes, err := nativeQuantity(value)
		if err != nil {
			return guard.Budget{}, err
		}
		if hostReserved, err = nativeAdd(hostReserved, bytes); err != nil {
			return guard.Budget{}, err
		}
	}
	reserved, err := nativeAdd(hostReserved, hard)
	if err != nil {
		return guard.Budget{}, err
	}
	if reserved >= physical {
		return guard.Budget{}, errors.New("node memory reservations exhaust physical memory")
	}
	// The host's configured kube+system reservation is its declared future
	// envelope. In the physical branch H = max(observed essential, that
	// envelope), so normal host growth inside it cannot push availability under
	// eviction while BE idles. Allocatable subtracts the same reservation in
	// the other min() branch only; it is never subtracted twice.
	return guard.Budget{
		Physical:           physical,
		Allocatable:        physical - reserved,
		Essential:          max(occupied-deduction, hostReserved),
		Eviction:           max(hard, soft),
		Available:          min(before.available, after.available),
		ProtectedResident:  min(first.protected, second.protected),
		ObservedResident:   min(first.observed, second.observed),
		BestEffortResident: min(first.bestEffort, second.bestEffort),
	}, nil
}

func nativeEvictionThreshold(thresholds map[string]string, physical uint64) (uint64, error) {
	value, ok := thresholds["memory.available"]
	if !ok {
		return 0, nil
	}
	return nativeThreshold(value, physical)
}

func nativeAdd(a, b uint64) (uint64, error) {
	if a > math.MaxUint64-b {
		return 0, errors.New("memory accounting overflow")
	}
	return a + b, nil
}

func nativeSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}
