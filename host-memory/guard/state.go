package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const stateVersion = 3
const stateName = "reservations.json"

// record is the single durable contract. Paths are relative to the cgroup
// root. Envelope/Floor are the admitted, kernel-enforced values; Intent and
// IntentFloor are an in-flight first admission (Pending) or protected growth
// (Admitted) that already counts as credit but was never returned as granted.
type record struct {
	ID          string `json:"id"`
	Tier        Tier   `json:"tier"`
	State       State  `json:"state"`
	Original    string `json:"original"`
	OriginalID  string `json:"original_identity,omitempty"`
	Parent      string `json:"parent"`
	ParentID    string `json:"parent_identity,omitempty"`
	Envelope    uint64 `json:"envelope"`
	Floor       uint64 `json:"floor"`
	Intent      uint64 `json:"intent,omitempty"`
	IntentFloor uint64 `json:"intent_floor,omitempty"`
	Boot        string `json:"boot_id"`
}

func (r record) credit() uint64 { return max(r.Envelope, r.Intent) }

type diskState struct {
	Version     int      `json:"version"`
	Root        string   `json:"cgroup_root"`
	PodRoot     string   `json:"pod_root"`
	BootID      string   `json:"boot_id"`
	AggregateID string   `json:"aggregate_identity,omitempty"`
	Records     []record `json:"reservations"`
}

type stateStore struct {
	dir         string
	root        string
	podRoot     string
	bootID      string
	existing    bool
	priorBoot   bool
	aggregateID string
	records     map[string]record
}

func openState(dir, root, podRoot, bootID string) (*stateStore, error) {
	// This is a local runtime-owned directory, not a tenant-provided mount. Walk
	// every component without symlinks and reject group/world-writable parents.
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(dir, current), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("host-memory: unsafe state directory ancestry")
		}
		// /tmp's sticky ownership does not protect a child directory from replacement
		// by its owner; accept it only as an ancestor, never as the actual state dir.
		if info.Mode().Perm()&0022 != 0 && !(current != dir && info.Mode()&os.ModeSticky != 0) {
			return nil, fmt.Errorf("host-memory: writable state ancestor %s", current)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("host-memory: state directory must be private and runtime-owned")
	}
	s := &stateStore{dir: dir, root: root, podRoot: podRoot, bootID: bootID, records: make(map[string]record)}
	file, err := openNoFollow(filepath.Join(dir, stateName), syscall.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	fileStat, ok := fileInfo.Sys().(*syscall.Stat_t)
	if !ok || !fileInfo.Mode().IsRegular() || fileStat.Uid != uint32(os.Geteuid()) || fileInfo.Mode().Perm()&0077 != 0 || fileInfo.Size() > 16<<20 {
		return nil, errors.New("host-memory: unsafe reservation state file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<20))
	decoder.DisallowUnknownFields()
	var d diskState
	if err = decoder.Decode(&d); err != nil {
		return nil, fmt.Errorf("host-memory: corrupt reservation state: %w", err)
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("host-memory: trailing reservation state")
	}
	if d.Version != stateVersion || d.Root != root || d.PodRoot != podRoot {
		return nil, errors.New("host-memory: state schema/root ownership mismatch")
	}
	if d.BootID == "" {
		return nil, errors.New("host-memory: durable state lacks kernel boot identity")
	}
	for _, r := range d.Records {
		if err = validateRecord(r, podRoot); err != nil {
			return nil, err
		}
		if r.State != StateReleased && r.Boot != d.BootID {
			return nil, errors.New("host-memory: live contract from another kernel boot")
		}
		if _, exists := s.records[r.ID]; exists {
			return nil, errors.New("host-memory: duplicate durable identity")
		}
		s.records[r.ID] = r
	}
	if err = validateOverlap(s.records); err != nil {
		return nil, err
	}
	s.aggregateID = d.AggregateID
	if len(s.records) > 0 && s.aggregateID == "" {
		return nil, errors.New("host-memory: reservation state lacks aggregate ownership")
	}
	// A new kernel cannot retain any old guest process or its charges. The old
	// file stays authoritative until Open proves the new aggregate adoptable and
	// commits prior-boot tombstones; a runtime restart never receives this reset.
	s.priorBoot = d.BootID != bootID
	s.existing = true
	return s, nil
}

func openNoFollow(p string, flags int) (*os.File, error) {
	fd, err := syscall.Open(p, flags|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(fd), p), nil
}

// commit atomically replaces the durable state, then the in-memory view.
func (s *stateStore) commit(records map[string]record, aggregateID string) error {
	for _, r := range records {
		if err := validateRecord(r, s.podRoot); err != nil {
			return err
		}
	}
	if err := validateOverlap(records); err != nil {
		return err
	}
	d := diskState{Version: stateVersion, Root: s.root, PodRoot: s.podRoot, BootID: s.bootID, AggregateID: aggregateID, Records: make([]record, 0, len(records))}
	for _, r := range records {
		d.Records = append(d.Records, r)
	}
	sort.Slice(d.Records, func(i, j int) bool { return d.Records[i].ID < d.Records[j].ID })
	if err := s.persist(d); err != nil {
		return err
	}
	s.records, s.aggregateID, s.existing = records, aggregateID, true
	return nil
}

func (s *stateStore) put(r record) error {
	records := make(map[string]record, len(s.records)+1)
	for key, value := range s.records {
		records[key] = value
	}
	records[r.ID] = r
	return s.commit(records, s.aggregateID)
}

func (s *stateStore) remove(id string) error {
	records := make(map[string]record, len(s.records))
	for key, value := range s.records {
		if key != id {
			records[key] = value
		}
	}
	return s.commit(records, s.aggregateID)
}

func (s *stateStore) persist(d diskState) error {
	bytes, err := json.Marshal(d)
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	// A fresh exclusive temporary file avoids reuse of a planted symlink and
	// leaves the previous complete state intact until rename commits the update.
	tmp, err := os.CreateTemp(s.dir, ".reservation-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(bytes)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(s.dir, stateName)); err != nil {
		return err
	}
	directory, err := openNoFollow(s.dir, syscall.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return fmt.Errorf("host-memory: durable state directory sync: %w", err)
	}
	return nil
}

func validateRecord(r record, podRoot string) error {
	invalid := func(reason string) error {
		return fmt.Errorf("host-memory: invalid durable contract %q: %s", r.ID, reason)
	}
	if err := validateRequest(r.ID, r.Tier, 1, 1); err != nil {
		return err
	}
	if p, err := cleanParent(r.Original); err != nil || p != r.Original || !strings.HasPrefix(p, podRoot+"/") {
		return invalid("original pod parent")
	}
	if r.Parent != privateParent(r.Tier, r.ID, r.Original) {
		return invalid("private parent")
	}
	if r.Boot == "" || r.Envelope > math.MaxInt64 || r.Intent > math.MaxInt64 {
		return invalid("boot or byte bound")
	}
	if r.Tier == Protected && r.OriginalID == "" {
		return invalid("protected original identity")
	}
	admitted := r.Envelope > 0 && r.Floor > 0 && r.Floor <= r.Envelope && r.ParentID != ""
	idle := r.Intent == 0 && r.IntentFloor == 0
	switch r.State {
	case StatePending:
		if r.Envelope != 0 || r.Floor != 0 || r.Intent == 0 || r.IntentFloor == 0 || r.IntentFloor > r.Intent || r.ParentID != "" {
			return invalid("pending admission")
		}
	case StateAdmitted:
		growth := r.Tier == Protected && r.Intent > r.Envelope && r.IntentFloor > 0 && r.IntentFloor <= r.Intent
		if !admitted || !(idle || growth) {
			return invalid("admitted contract")
		}
	case StateReleasing:
		if !admitted || !idle {
			return invalid("releasing contract")
		}
	case StateReleased:
		if !idle {
			return invalid("released tombstone")
		}
	default:
		return invalid("state")
	}
	return nil
}

// Multiple holders may share one kubelet pod parent; distinct live originals
// must never nest, otherwise one envelope could be credited inside another.
func validateOverlap(records map[string]record) error {
	parents := make(map[string]bool, len(records))
	for _, r := range records {
		if r.State != StateReleased {
			parents[r.Original] = true
		}
	}
	for parent := range parents {
		for ancestor := path.Dir(parent); ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
			if parents[ancestor] {
				return errors.New("host-memory: overlapping original pod identities")
			}
		}
	}
	return nil
}

func kernelBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !validBootID(id) {
		return "", errors.New("host-memory: invalid kernel boot identity")
	}
	return id, nil
}

func validBootID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "/\x00\n\r ")
}
