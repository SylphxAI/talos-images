package guard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// modelKernel models cgroup-v2 observations the guard depends on: hierarchical
// charges/population, per-creation identity, rmdir refused while populated or
// with children, residual charges reparented on rmdir, and limit readback.
// Limit/reclaim/kill writes never release bytes by themselves; tests control
// charges and population separately (autoKill models a successful kill).
type modelGroup struct {
	files     map[string]string
	current   uint64
	populated bool
	id        int
}

type modelKernel struct {
	mu          sync.Mutex
	locked      bool
	groups      map[string]*modelGroup
	serial      int
	changed     chan struct{}
	log         []string
	reclaim     func(context.Context, string, uint64) error
	autoKill    bool
	readFault   map[string]string
	writeFault  map[string]error
	killed      chan struct{}
	killOnce    sync.Once
	waiting     chan string
	rootResidue uint64
}

type modelLock struct{ k *modelKernel }

func (l modelLock) Close() error {
	l.k.mu.Lock()
	defer l.k.mu.Unlock()
	l.k.locked = false
	return nil
}

func newModel() *modelKernel {
	return &modelKernel{groups: make(map[string]*modelGroup), changed: make(chan struct{}), readFault: make(map[string]string),
		writeFault: make(map[string]error), killed: make(chan struct{}), waiting: make(chan string, 64)}
}

func (k *modelKernel) Lock() (io.Closer, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.locked {
		return nil, errors.New("node already locked")
	}
	k.locked = true
	return modelLock{k}, nil
}

func (k *modelKernel) Validate(ctx context.Context) error { return ctx.Err() }

func (k *modelKernel) Exists(ctx context.Context, p string) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.groups[p]
	return ok, ctx.Err()
}

func (k *modelKernel) Identity(ctx context.Context, p string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	g, ok := k.groups[p]
	if !ok {
		return "", fmt.Errorf("identity %s: %w", p, os.ErrNotExist)
	}
	return "cgroup-" + strconv.Itoa(g.id), nil
}

func (k *modelKernel) createLocked(p string) error {
	if _, ok := k.groups[p]; ok {
		return os.ErrExist
	}
	if parent := path.Dir(p); parent != "." {
		if _, ok := k.groups[parent]; !ok {
			return os.ErrNotExist
		}
	}
	k.serial++
	k.groups[p] = &modelGroup{id: k.serial, files: map[string]string{"cgroup.type": "domain", "cgroup.controllers": "memory", "cgroup.subtree_control": "",
		"memory.max": "max", "memory.high": "max", "memory.swap.max": "max", "memory.oom.group": "0"}}
	return nil
}

func (k *modelKernel) Mkdir(ctx context.Context, p string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	k.log = append(k.log, "mkdir "+p)
	return k.createLocked(p)
}

func (k *modelKernel) Children(ctx context.Context, p string) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.groups[p]; !ok {
		return nil, os.ErrNotExist
	}
	var names []string
	for name := range k.groups {
		if path.Dir(name) == p {
			names = append(names, path.Base(name))
		}
	}
	return names, ctx.Err()
}

func (k *modelKernel) usageLocked(p string) (uint64, bool) {
	var current uint64
	populated := false
	for name, g := range k.groups {
		if name == p || strings.HasPrefix(name, p+"/") {
			current += g.current
			populated = populated || g.populated
		}
	}
	return current, populated
}

func (k *modelKernel) Read(ctx context.Context, p string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if text, ok := k.readFault[p]; ok {
		return text, nil
	}
	group, ok := k.groups[path.Dir(p)]
	if !ok {
		return "", fmt.Errorf("read %s: %w", p, os.ErrNotExist)
	}
	current, populated := k.usageLocked(path.Dir(p))
	switch path.Base(p) {
	case "memory.current":
		return strconv.FormatUint(current, 10), nil
	case "cgroup.events":
		if populated {
			return "populated 1\nfrozen 0\n", nil
		}
		return "populated 0\nfrozen 0\n", nil
	default:
		value, ok := group.files[path.Base(p)]
		if !ok {
			return "", os.ErrNotExist
		}
		return value, nil
	}
}

func (k *modelKernel) signalLocked() { close(k.changed); k.changed = make(chan struct{}) }

func (k *modelKernel) Write(ctx context.Context, p, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	k.log = append(k.log, "write "+p+"="+value)
	if err := k.writeFault[p]; err != nil {
		return err
	}
	group, ok := k.groups[path.Dir(p)]
	if !ok {
		return os.ErrNotExist
	}
	if path.Base(p) == "cgroup.kill" {
		if p != aggregate+"/cgroup.kill" {
			return errors.New("protected kill")
		}
		k.killOnce.Do(func() { close(k.killed) })
		if k.autoKill {
			for name, g := range k.groups {
				if name == aggregate || strings.HasPrefix(name, aggregate+"/") {
					g.current, g.populated = 0, false
				}
			}
			k.signalLocked()
		}
		return nil
	}
	if value == "+memory" {
		value = "memory"
	}
	group.files[path.Base(p)] = value
	return nil
}

func (k *modelKernel) Reclaim(ctx context.Context, p string, n uint64) error {
	k.mu.Lock()
	k.log = append(k.log, fmt.Sprintf("reclaim %s=%d", p, n))
	fn := k.reclaim
	k.mu.Unlock()
	if fn != nil {
		return fn(ctx, p, n)
	}
	return errors.New("not reclaimed")
}

func (k *modelKernel) WaitUnpopulated(ctx context.Context, p string) error {
	select {
	case k.waiting <- p:
	default:
	}
	for {
		k.mu.Lock()
		_, exists := k.groups[p]
		_, populated := k.usageLocked(p)
		changed := k.changed
		k.mu.Unlock()
		if !exists {
			return fmt.Errorf("wait %s: %w", p, os.ErrNotExist)
		}
		if !populated {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (k *modelKernel) Remove(ctx context.Context, p string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	g, ok := k.groups[p]
	if !ok {
		return os.ErrNotExist
	}
	if _, populated := k.usageLocked(p); populated {
		return errors.New("EBUSY: populated")
	}
	for name := range k.groups {
		if path.Dir(name) == p {
			return errors.New("EBUSY: live children")
		}
	}
	k.reparentLocked(p, g.current)
	delete(k.groups, p)
	k.log = append(k.log, "remove "+p)
	k.signalLocked()
	return nil
}

func (k *modelKernel) reparentLocked(p string, bytes uint64) {
	if parent, ok := k.groups[path.Dir(p)]; ok {
		parent.current += bytes
	} else {
		k.rootResidue += bytes
	}
}

// kubelet creates/updates a kubelet-owned original pod cgroup.
func (k *modelKernel) kubelet(original string, limit uint64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	parts := strings.Split(original, "/")
	for i := range parts {
		p := strings.Join(parts[:i+1], "/")
		if _, ok := k.groups[p]; !ok {
			_ = k.createLocked(p)
		}
		if i < len(parts)-1 {
			k.groups[p].files["cgroup.subtree_control"] = "memory"
		}
	}
	k.groups[original].files["memory.max"] = strconv.FormatUint(limit, 10)
}

// destroy models kubelet pcm.Destroy: its subtree is gone, residue reparents.
func (k *modelKernel) destroy(original string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for name, g := range k.groups {
		if name == original || strings.HasPrefix(name, original+"/") {
			k.reparentLocked(original, g.current)
			delete(k.groups, name)
		}
	}
	k.signalLocked()
}

func (k *modelKernel) create(p string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.createLocked(p); err != nil {
		panic(err)
	}
}

func (k *modelKernel) charge(p string, n uint64, populated bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.groups[p].current, k.groups[p].populated = n, populated
	k.signalLocked()
}

func (k *modelKernel) setFile(p, name, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.groups[p].files[name] = value
}

func (k *modelKernel) file(p, name string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.groups[p].files[name]
}

func (k *modelKernel) number(p string) uint64 {
	n, _ := strconv.ParseUint(k.file(path.Dir(p), path.Base(p)), 10, 64)
	return n
}

func (k *modelKernel) current(p string) uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	n, _ := k.usageLocked(p)
	return n
}

func (k *modelKernel) has(p string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.groups[p]
	return ok
}

func (k *modelKernel) logs() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.log...)
}

func (k *modelKernel) wasKilled() bool {
	select {
	case <-k.killed:
		return true
	default:
		return false
	}
}

type budgetProbe struct {
	mu     sync.Mutex
	budget Budget
	err    error
	calls  [][]string
}

func (b *budgetProbe) read(originals []string) (Budget, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, append([]string(nil), originals...))
	return b.budget, b.err
}

func (b *budgetProbe) set(change func(*Budget, *error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change(&b.budget, &b.err)
}

func (b *budgetProbe) last() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) == 0 {
		return nil
	}
	return b.calls[len(b.calls)-1]
}

func (b *budgetProbe) count() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.calls) }

const podRoot = "kubepods"

func configFor(t *testing.T, k *modelKernel) (Config, *budgetProbe) {
	t.Helper()
	probe := &budgetProbe{budget: Budget{Physical: 1200, Allocatable: 1000, Essential: 100, Eviction: 100, Available: 1200}}
	return Config{CgroupRoot: "/sys/fs/cgroup", PodRoot: "/" + podRoot, StateDir: path.Join(t.TempDir(), "state"), BootID: "test-boot-one", Kernel: k,
		NodeBudget: probe.read, OperationTimeout: time.Second, ReclaimTimeout: 10 * time.Millisecond}, probe
}

func openFor(t *testing.T, k *modelKernel) (*Guard, Config, *budgetProbe) {
	t.Helper()
	c, probe := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g, c, probe
}

func originalOf(id string) string { return podRoot + "/pods/" + id }

func privateOf(tier Tier, id string) string { return privateParent(tier, id, originalOf(id)) }

func reserveAt(t *testing.T, g *Guard, k *modelKernel, id string, tier Tier, envelope, floor uint64, original string) Contract {
	t.Helper()
	k.kubelet(original, envelope)
	c, err := g.Reserve(context.Background(), id, tier, envelope, floor, "/"+original)
	if err != nil {
		t.Fatal(err)
	}
	if c.State != StateAdmitted || c.Envelope != envelope || c.Original != "/"+original {
		t.Fatalf("unexpected contract %+v", c)
	}
	return c
}

func reserveFor(t *testing.T, g *Guard, k *modelKernel, id string, tier Tier, envelope, floor uint64) string {
	t.Helper()
	return strings.TrimPrefix(reserveAt(t, g, k, id, tier, envelope, floor, originalOf(id)).Parent, "/")
}

func reserveAsync(g *Guard, ctx context.Context, id string, envelope uint64) chan error {
	done := make(chan error, 1)
	go func() {
		_, err := g.Reserve(ctx, id, Protected, envelope, envelope, "/"+originalOf(id))
		done <- err
	}()
	return done
}

func expectError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("unexpected admission/release success")
	}
}

func diskRecords(t *testing.T, c Config, boot string) map[string]record {
	t.Helper()
	s, err := openState(c.StateDir, c.CgroupRoot, podRoot, boot)
	if err != nil {
		t.Fatal(err)
	}
	return s.records
}

func onlyAggregateKills(t *testing.T, k *modelKernel) {
	t.Helper()
	for _, entry := range k.logs() {
		if strings.Contains(entry, "cgroup.kill") && entry != "write "+aggregate+"/cgroup.kill=1" {
			t.Fatalf("kill escaped best-effort aggregate: %s", entry)
		}
	}
}

func TestRoomExactlyOnceAndOverflow(t *testing.T) {
	cases := []struct {
		b    Budget
		want uint64
		bad  bool
	}{
		{Budget{Physical: 1200, Allocatable: 1000, Essential: 100, Eviction: 100}, 1000, false}, // not 800: allocatable excludes both.
		{Budget{Physical: 1200, Allocatable: 1100, Essential: 100, Eviction: 200}, 900, false},
		{Budget{Physical: 1200, Allocatable: 1000, Essential: 1000, Eviction: 300}, 0, false},
		{Budget{Physical: 1200, Allocatable: 0, Essential: 100, Eviction: 100}, 0, true},
		{Budget{Physical: 1200, Allocatable: 1300, Essential: 100, Eviction: 100}, 0, true},
		{Budget{Physical: 1200, Allocatable: 1000, Essential: math.MaxUint64, Eviction: 1}, 0, true},
		{Budget{Physical: 1200, Allocatable: 1000, Available: 1201}, 0, true},
		{Budget{Physical: 1200, Allocatable: 1000, BestEffortResident: 1201}, 0, true},
	}
	for _, test := range cases {
		got, err := UsableRoom(test.b)
		if (err != nil) != test.bad || (!test.bad && got != test.want) {
			t.Fatalf("%+v: got %d, %v", test.b, got, err)
		}
	}
}

func TestEnvelopeOverheadIsNotAddedAgainAndKubeletOriginalIsNeverWritten(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	// Adapter gives guest 300 + overhead 20 exactly once, floor 100 + 20.
	be := reserveFor(t, g, k, "be", BestEffort, 320, 120)
	protected := reserveFor(t, g, k, "protected", Protected, 320, 320)
	for name, want := range map[string]string{"memory.max": "320", "memory.high": "120", "memory.swap.max": "0", "memory.oom.group": "1"} {
		if got := k.file(be, name); got != want {
			t.Fatalf("best-effort private %s=%s", name, got)
		}
	}
	// Protected private: finite envelope only; failure scope never widened.
	for name, want := range map[string]string{"memory.max": "320", "memory.high": "320", "memory.swap.max": "max", "memory.oom.group": "0"} {
		if got := k.file(protected, name); got != want {
			t.Fatalf("protected private %s=%s", name, got)
		}
	}
	// One best-effort pod OOM fails that pod only, never every best-effort pod.
	for name, want := range map[string]string{"memory.max": "680", "memory.high": "120", "memory.swap.max": "0", "memory.oom.group": "0"} {
		if got := k.file(aggregate, name); got != want {
			t.Fatalf("aggregate %s=%s", name, got)
		}
	}
	for _, entry := range k.logs() {
		if strings.HasPrefix(entry, "write "+originalOf("protected")+"/memory.") || entry == "mkdir "+originalOf("protected") || entry == "mkdir "+originalOf("be") {
			t.Fatalf("kubelet original rewritten or created: %s", entry)
		}
	}
	if k.file(originalOf("protected"), "memory.max") != "320" {
		t.Fatal("kubelet original limit changed")
	}
}

func TestProtectedGrowthCannotCrossUnreleasedFence(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 900, 200)
	protected := reserveFor(t, g, k, "p", Protected, 200, 200)
	k.charge(be, 700, true)
	k.kubelet(originalOf("p"), 600)
	done := reserveAsync(g, context.Background(), "p", 600)
	<-k.killed
	if got := k.number(aggregate + "/memory.max"); got != 400 {
		t.Fatalf("bound before kill %d", got)
	}
	if got := k.number(protected + "/memory.max"); got != 200 {
		t.Fatalf("protected max grew before release: %d", got)
	}
	select {
	case err := <-done:
		t.Fatalf("protected fence returned while charged: %v", err)
	default:
	}
	k.charge(be, 0, false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := k.number(protected + "/memory.max"); got != 600 {
		t.Fatalf("protected max after fence %d", got)
	}
	onlyAggregateKills(t, k)
}

func TestResizeUpDownUpFollowsKubeletEnvelopeWithoutRatchet(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	protected := reserveFor(t, g, k, "p", Protected, 300, 300)
	original := "/" + originalOf("p")
	// Shrink before kubelet lowered its pod cgroup proves nothing: no credit.
	_, err := g.Reserve(context.Background(), "p", Protected, 100, 100, original)
	expectError(t, err)
	if c, _ := g.Contract("p"); c.Envelope != 300 || k.number(aggregate+"/memory.max") != 700 {
		t.Fatalf("unproven shrink credited: %+v", c)
	}
	// Kubelet's original now enforces 100 and charges fit: credit returns.
	k.setFile(originalOf("p"), "memory.max", "100")
	if _, err = g.Reserve(context.Background(), "p", Protected, 100, 100, original); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 900 || k.number(protected+"/memory.max") != 100 {
		t.Fatal("enforced shrink did not return credit")
	}
	// Growth again is an admission of the authoritative pod envelope, not a delta.
	k.setFile(originalOf("p"), "memory.max", "300")
	if c, err := g.Reserve(context.Background(), "p", Protected, 300, 300, original); err != nil || c.Envelope != 300 {
		t.Fatalf("regrowth %+v %v", c, err)
	}
	if k.number(aggregate+"/memory.max") != 700 {
		t.Fatal("resize cycle ratcheted the envelope")
	}
	// Charges above a requested smaller envelope retain credit.
	k.charge(protected, 200, true)
	k.setFile(originalOf("p"), "memory.max", "100")
	_, err = g.Reserve(context.Background(), "p", Protected, 100, 100, original)
	expectError(t, err)
	if c, _ := g.Contract("p"); c.Envelope != 300 || k.number(protected+"/memory.max") != 300 {
		t.Fatal("charged shrink lowered protected cap or credit")
	}
}

func TestReclaimFailureUnderReclaimAndStallKillOnlyBestEffort(t *testing.T) {
	for _, mode := range []string{"failure", "under", "stall", "success"} {
		t.Run(mode, func(t *testing.T) {
			k := newModel()
			k.autoKill = true
			g, _, _ := openFor(t, k)
			be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
			k.charge(be, 900, true)
			k.reclaim = func(ctx context.Context, p string, n uint64) error {
				if p != aggregate {
					t.Error("reclaim escaped best-effort")
				}
				switch mode {
				case "under":
					k.charge(be, 800, true)
					return nil
				case "stall":
					<-ctx.Done()
					return ctx.Err()
				case "success":
					k.charge(be, 400, true)
					return nil
				default:
					return errors.New("reclaim failed")
				}
			}
			reserveFor(t, g, k, "p", Protected, 500, 500)
			if k.wasKilled() != (mode != "success") {
				t.Fatalf("kill=%v mode=%s", k.wasKilled(), mode)
			}
			onlyAggregateKills(t, k)
		})
	}
}

func TestPostKillResidueIsReclaimedAndJudgedAgainstBoundNotZero(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
	k.charge(be, 900, true)
	k.reclaim = func(ctx context.Context, p string, n uint64) error {
		if !k.wasKilled() {
			return errors.New("reclaim failed")
		}
		k.charge(be, 300, false)
		return nil
	}
	k.kubelet(originalOf("p"), 500)
	done := reserveAsync(g, context.Background(), "p", 500)
	<-k.killed
	// Processes exited; page cache above the bound stays charged.
	k.charge(be, 700, false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if k.current(aggregate) != 300 || k.number(privateOf(Protected, "p")+"/memory.max") != 500 {
		t.Fatal("protected admitted without actual residue at or below the bound")
	}
	log := k.logs()
	kill := slices.Index(log, "write "+aggregate+"/cgroup.kill=1")
	if kill < 0 || !slices.ContainsFunc(log[kill:], func(s string) bool { return strings.HasPrefix(s, "reclaim "+aggregate) }) {
		t.Fatal("residual above bound was not reclaimed after kill")
	}
}

func TestUnreclaimableResidueFailsAndRollsBackIntent(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
	k.charge(be, 900, true)
	k.kubelet(originalOf("p"), 500)
	done := reserveAsync(g, context.Background(), "p", 500)
	<-k.killed
	k.charge(be, 700, false)
	expectError(t, <-done)
	if _, ok := g.Contract("p"); ok {
		t.Fatal("failed admission left a contract")
	}
	if k.has(privateOf(Protected, "p")) || !k.has(originalOf("p")) {
		t.Fatal("rollback left a private cgroup or touched the kubelet original")
	}
	if _, ok := diskRecords(t, c, c.BootID)["p"]; ok {
		t.Fatal("rejected intent remains durable")
	}
}

func TestRacingShimAfterKillIsConfinedByHardBound(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
	k.charge(be, 900, true)
	k.kubelet(originalOf("p"), 500)
	done := reserveAsync(g, context.Background(), "p", 500)
	<-k.killed
	// A shim entered after the kill: still populated, but under the hard max.
	k.charge(be, 300, true)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRejectedBeforeWALKeepsExistingContractUsable(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	reserveFor(t, g, k, "p1", Protected, 600, 600)
	k.kubelet(originalOf("p2"), 500)
	_, err := g.Reserve(context.Background(), "p2", Protected, 500, 500, "/"+originalOf("p2"))
	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("want ErrNoRoom, got %v", err)
	}
	if _, ok := g.Contract("p2"); ok || k.has(privateOf(Protected, "p2")) {
		t.Fatal("rejected demand was recorded or delegated")
	}
	if _, ok := diskRecords(t, c, c.BootID)["p2"]; ok {
		t.Fatal("rejected demand reached the WAL")
	}
	if _, err = g.Reserve(context.Background(), "p1", Protected, 600, 600, "/"+originalOf("p1")); err != nil {
		t.Fatal(err)
	}
	if contract, err := g.Fence(context.Background(), "p1"); err != nil || contract.State != StateAdmitted {
		t.Fatalf("existing contract unusable: %+v %v", contract, err)
	}
}

func TestCancelledReserveRollsBackIntent(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		k := newModel()
		g, c, _ := openFor(t, k)
		be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
		k.charge(be, 900, true)
		k.kubelet(originalOf("p"), 600)
		ctx, cancel := context.WithCancel(context.Background())
		done := reserveAsync(g, ctx, "p", 600)
		<-k.killed
		cancel()
		expectError(t, <-done)
		if _, ok := g.Contract("p"); ok || k.has(privateOf(Protected, "p")) {
			t.Fatal("cancelled admission retained intent")
		}
		if _, ok := diskRecords(t, c, c.BootID)["p"]; ok {
			t.Fatal("cancelled intent remains durable")
		}
		if err := g.Reconcile(context.Background()); err != nil || k.number(aggregate+"/memory.max") != 1000 {
			t.Fatalf("cancelled intent still squeezes best-effort: %v", err)
		}
	})
	t.Run("growth", func(t *testing.T) {
		k := newModel()
		g, c, _ := openFor(t, k)
		be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
		protected := reserveFor(t, g, k, "p", Protected, 200, 200)
		k.charge(be, 790, true)
		k.kubelet(originalOf("p"), 600)
		ctx, cancel := context.WithCancel(context.Background())
		done := reserveAsync(g, ctx, "p", 600)
		<-k.killed
		cancel()
		expectError(t, <-done)
		if contract, _ := g.Contract("p"); contract.State != StateAdmitted || contract.Envelope != 200 || k.number(protected+"/memory.max") != 200 {
			t.Fatalf("cancelled growth changed the committed contract: %+v", contract)
		}
		if r := diskRecords(t, c, c.BootID)["p"]; r.Envelope != 200 || r.Intent != 0 {
			t.Fatalf("cancelled growth intent durable: %+v", r)
		}
		// The previously committed contract remains usable.
		k.setFile(originalOf("p"), "memory.max", "200")
		if _, err := g.Fence(context.Background(), "p"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGrowthCapWriteOutcomeDecidesRollbackOrConservativeAdmission(t *testing.T) {
	t.Run("write-rejected", func(t *testing.T) {
		k := newModel()
		g, _, _ := openFor(t, k)
		protected := reserveFor(t, g, k, "p", Protected, 200, 200)
		k.kubelet(originalOf("p"), 600)
		k.writeFault[protected+"/memory.max"] = errors.New("kernel rejected")
		_, err := g.Reserve(context.Background(), "p", Protected, 600, 600, "/"+originalOf("p"))
		expectError(t, err)
		if contract, _ := g.Contract("p"); contract.Envelope != 200 || k.number(aggregate+"/memory.max") != 800 {
			t.Fatalf("uncommitted cap growth kept credit: %+v", contract)
		}
	})
	t.Run("readback-unknown", func(t *testing.T) {
		k := newModel()
		g, _, _ := openFor(t, k)
		protected := reserveFor(t, g, k, "p", Protected, 200, 200)
		k.kubelet(originalOf("p"), 600)
		k.readFault[protected+"/memory.max"] = "700"
		_, err := g.Reserve(context.Background(), "p", Protected, 600, 600, "/"+originalOf("p"))
		expectError(t, err)
		if contract, _ := g.Contract("p"); contract.Envelope != 600 || k.number(aggregate+"/memory.max") != 400 {
			t.Fatalf("possibly committed cap growth lost conservative credit: %+v", contract)
		}
		delete(k.readFault, protected+"/memory.max")
		// Retained state is an admitted contract; an unchanged request is a
		// re-fence, never a re-admission of the rejected demand.
		if _, err = g.Reserve(context.Background(), "p", Protected, 600, 600, "/"+originalOf("p")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConcurrentProtectedCreatesShareOneBudget(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	const n = 20
	for i := range n {
		k.kubelet(originalOf(fmt.Sprintf("p%d", i)), 100)
	}
	results := make(chan error, n)
	for i := range n {
		go func() { results <- <-reserveAsync(g, context.Background(), fmt.Sprintf("p%d", i), 100) }()
	}
	accepted := 0
	for range n {
		if err := <-results; err == nil {
			accepted++
		} else if !errors.Is(err, ErrNoRoom) {
			t.Fatal(err)
		}
	}
	if accepted != 10 || len(g.Contracts()) != 10 {
		t.Fatalf("accepted=%d contracts=%d", accepted, len(g.Contracts()))
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("aggregate gained nonexistent room")
	}
}

func TestNodeWideLockAndRuntimeRestartRecovery(t *testing.T) {
	k := newModel()
	g, c, probe := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 600, 100)
	reserveFor(t, g, k, "p", Protected, 300, 300)
	_, err := Open(c)
	expectError(t, err)
	other := c
	other.StateDir = t.TempDir()
	_, err = Open(other)
	expectError(t, err)
	k.charge(be, 650, true)
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	calls := probe.count()
	recovered, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if probe.count() == calls {
		t.Fatal("live contracts recovered without a live budget")
	}
	if p, _ := recovered.Contract("p"); p.Envelope != 300 || p.State != StateAdmitted {
		t.Fatalf("restart lost contract %+v", p)
	}
	if b, _ := recovered.Contract("be"); b.Envelope != 600 {
		t.Fatal("restart lost best-effort contract")
	}
	if k.number(aggregate+"/memory.max") != 700 || k.wasKilled() {
		t.Fatal("restart formula wrong or killed within-bound guests")
	}
	if _, err = recovered.Fence(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRollsBackUncommittedGrowthIntent(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
	protected := reserveFor(t, g, k, "p", Protected, 200, 200)
	// Crash after the durable growth intent, before the cap write.
	r := g.store.records["p"]
	r.Intent, r.IntentFloor = 600, 600
	if err := g.store.put(r); err != nil {
		t.Fatal(err)
	}
	k.charge(be, 700, true)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if contract, _ := recovered.Contract("p"); contract.Envelope != 200 || k.number(protected+"/memory.max") != 200 {
		t.Fatalf("restart re-admitted uncommitted intent: %+v", contract)
	}
	if k.wasKilled() || k.number(aggregate+"/memory.max") != 800 {
		t.Fatal("rolled-back intent still fenced best-effort")
	}
}

func TestRestartRetainsCommittedGrowthConservatively(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	protected := reserveFor(t, g, k, "p", Protected, 200, 200)
	r := g.store.records["p"]
	r.Intent, r.IntentFloor = 600, 600
	if err := g.store.put(r); err != nil {
		t.Fatal(err)
	}
	// The cap write committed and charges already exceed the old envelope.
	k.setFile(protected, "memory.max", "600")
	k.charge(protected, 400, true)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if contract, _ := recovered.Contract("p"); contract.Envelope != 600 || k.number(protected+"/memory.max") != 600 {
		t.Fatalf("committed growth lost or protected cap forced down: %+v", contract)
	}
	if k.number(aggregate+"/memory.max") != 400 {
		t.Fatal("committed growth not credited")
	}
}

func TestRestartSettlesPendingFirstAdmission(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(strconv.FormatBool(populated), func(t *testing.T) {
			k := newModel()
			g, c, _ := openFor(t, k)
			k.kubelet(originalOf("q"), 300)
			private := privateOf(Protected, "q")
			if err := g.store.put(record{ID: "q", Tier: Protected, State: StatePending, Original: originalOf("q"), OriginalID: "kubelet", Parent: private, Intent: 300, IntentFloor: 300, Boot: c.BootID}); err != nil {
				t.Fatal(err)
			}
			k.create(private)
			k.charge(private, 50, populated)
			if err := g.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			contract, ok := recovered.Contract("q")
			if populated {
				if !ok || contract.State != StateAdmitted || contract.Envelope != 300 || k.file(private, "memory.max") != "300" {
					t.Fatalf("populated pending not held and bounded conservatively: %+v", contract)
				}
				return
			}
			if ok || k.has(private) {
				t.Fatal("never-returned pending admission was not rolled back")
			}
			if k.current(originalOf("q")) != 50 || !k.has(originalOf("q")) {
				t.Fatal("residual charge not kept in kubelet original")
			}
		})
	}
}

func TestReleaseWaitsOutsideNodeLockAndKeepsResidueMeasured(t *testing.T) {
	k := newModel()
	g, _, probe := openFor(t, k)
	protected := reserveFor(t, g, k, "p", Protected, 300, 300)
	k.charge(protected, 200, true)
	done := make(chan error, 1)
	go func() { done <- g.Release(context.Background(), "p") }()
	for p := range k.waiting {
		if p == protected {
			break
		}
	}
	// The node lock is free while the release waits for the kernel event.
	reserveFor(t, g, k, "q", Protected, 300, 300)
	if contract, _ := g.Contract("p"); contract.State != StateReleasing || k.number(aggregate+"/memory.max") != 400 {
		t.Fatalf("held release lost credit: %+v", contract)
	}
	// Processes exited; page cache stays charged. Cleanup must not need zero.
	k.charge(protected, 50, false)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if contract, _ := g.Contract("p"); contract.State != StateReleased || k.has(protected) {
		t.Fatalf("unpopulated release not completed: %+v", contract)
	}
	if k.current(originalOf("p")) != 50 {
		t.Fatal("residue not reparented into the kubelet original")
	}
	if k.number(aggregate+"/memory.max") != 700 || !slices.Equal(probe.last(), []string{"/" + originalOf("q")}) {
		t.Fatalf("released original still reserved: %v", probe.last())
	}
	if err := g.Forget(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.Contract("p"); ok {
		t.Fatal("forgotten tombstone remains")
	}
}

func TestHeldReleaseIsRetryableAndNonFatalOnRestart(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	protected := reserveFor(t, g, k, "p", Protected, 300, 300)
	k.charge(protected, 100, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.Release(ctx, "p"); !errors.Is(err, ErrHeld) {
		t.Fatalf("want ErrHeld, got %v", err)
	}
	if contract, _ := g.Contract("p"); contract.State != StateReleasing || k.number(aggregate+"/memory.max") != 700 {
		t.Fatal("held release returned credit")
	}
	expectError(t, g.Forget(context.Background(), "p"))
	_, err := g.Reserve(context.Background(), "p", Protected, 300, 300, "/"+originalOf("p"))
	expectError(t, err)
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if contract, _ := recovered.Contract("p"); contract.State != StateReleasing {
		t.Fatal("restart lost held release")
	}
	k.charge(protected, 100, false)
	if err = recovered.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if contract, _ := recovered.Contract("p"); contract.State != StateReleased || k.number(aggregate+"/memory.max") != 1000 {
		t.Fatal("orphan unpopulated release did not settle")
	}
}

func TestKubeletRemovedOrReplacedPodCgroupSettlesWithoutPermanentHold(t *testing.T) {
	k := newModel()
	g, c, probe := openFor(t, k)
	reserveFor(t, g, k, "p", Protected, 300, 300)
	reserveFor(t, g, k, "q", Protected, 300, 300)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	// Kubelet destroys the terminated pod cgroup before RemovePodSandbox.
	k.destroy(originalOf("p"))
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if contract, _ := g.Contract("p"); contract.State != StateReleased {
		t.Fatal("restart kept a missing-path reservation")
	}
	if k.number(aggregate+"/memory.max") != 700 || !slices.Equal(probe.last(), []string{"/" + originalOf("q")}) {
		t.Fatal("removed pod still credited")
	}
	k.destroy(originalOf("q"))
	if err = g.Release(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if contract, _ := g.Contract("q"); contract.State != StateReleased || k.number(aggregate+"/memory.max") != 1000 {
		t.Fatal("live release of removed pod cgroup not credited")
	}
	reserveFor(t, g, k, "r", Protected, 300, 300)
	k.destroy(originalOf("r"))
	k.kubelet(originalOf("r"), 300)
	k.create(privateOf(Protected, "r")) // same path, different kernel identity
	if _, err = g.Fence(context.Background(), "r"); err == nil {
		t.Fatal("replaced private cgroup accepted as the admitted one")
	}
	if contract, _ := g.Contract("r"); contract.State != StateReleased {
		t.Fatal("replaced identity not settled")
	}
}

func TestPodRecreationSharesOneOriginalEnvelope(t *testing.T) {
	k := newModel()
	g, _, probe := openFor(t, k)
	pod := originalOf("pod")
	s1 := strings.TrimPrefix(reserveAt(t, g, k, "s1", Protected, 400, 400, pod).Parent, "/")
	s2 := strings.TrimPrefix(reserveAt(t, g, k, "s2", Protected, 400, 400, pod).Parent, "/")
	if s1 == s2 || !k.has(s1) || !k.has(s2) {
		t.Fatal("holders do not have independent private children")
	}
	if k.number(aggregate+"/memory.max") != 600 || !slices.Equal(probe.last(), []string{"/" + pod}) {
		t.Fatalf("holders summed instead of max per original: %v", probe.last())
	}
	k.charge(s1, 30, false)
	if err := g.Release(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if k.has(s1) || k.number(aggregate+"/memory.max") != 600 {
		t.Fatal("old holder release changed live holder credit")
	}
	if err := g.Release(context.Background(), "s2"); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 1000 {
		t.Fatal("last holder release kept credit")
	}
	bePod := originalOf("bepod")
	b1 := reserveAt(t, g, k, "b1", BestEffort, 200, 100, bePod).Parent
	b2 := reserveAt(t, g, k, "b2", BestEffort, 200, 100, bePod).Parent
	if b1 == b2 || !k.has(strings.TrimPrefix(b1, "/")) || !k.has(strings.TrimPrefix(b2, "/")) {
		t.Fatal("best-effort holders share a private child")
	}
}

func TestPartialUnlimitedOrForeignKubeletEnvelopeRefused(t *testing.T) {
	k := newModel()
	g, _, probe := openFor(t, k)
	probe.set(func(b *Budget, _ *error) { *b = Budget{Physical: 1 << 20, Allocatable: 1 << 20, Available: 1 << 20} })
	reserve := func(id string, envelope uint64, original string) error {
		_, err := g.Reserve(context.Background(), id, Protected, envelope, envelope, original)
		return err
	}
	k.kubelet(originalOf("unlimited"), 300)
	k.setFile(originalOf("unlimited"), "memory.max", "max")
	expectError(t, reserve("unlimited", 300, "/"+originalOf("unlimited")))
	k.kubelet(originalOf("partial"), 2*g.pageSize)
	expectError(t, reserve("partial", 3*g.pageSize, "/"+originalOf("partial")))
	expectError(t, reserve("missing", 300, "/"+originalOf("missing")))
	expectError(t, reserve("outside", 300, "/system.slice/outside"))
	expectError(t, reserve("private", 300, "/"+privateOf(Protected, "x")))
	k.kubelet(originalOf("rounded"), 8192)
	if err := reserve("rounded", 8192+g.pageSize-1, "/"+originalOf("rounded")); err != nil {
		t.Fatal(err)
	}
	k.kubelet(originalOf("page"), 8192)
	expectError(t, reserve("page", 8192+g.pageSize, "/"+originalOf("page")))
	if len(g.Contracts()) != 1 {
		t.Fatalf("refused envelopes recorded: %+v", g.Contracts())
	}
	for _, entry := range k.logs() {
		if strings.HasPrefix(entry, "mkdir ") && entry != "mkdir "+aggregate && entry != "mkdir "+privateOf(Protected, "rounded") {
			t.Fatalf("refused envelope delegated: %s", entry)
		}
	}
}

func TestUnchangedReserveSkipsAdmissionButStillFences(t *testing.T) {
	k := newModel()
	g, _, probe := openFor(t, k)
	reserveFor(t, g, k, "p", Protected, 300, 300)
	probe.set(func(b *Budget, _ *error) { b.Essential = 900 })
	if _, err := g.Reserve(context.Background(), "p", Protected, 300, 300, "/"+originalOf("p")); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("unchanged re-fence skipped the live bound")
	}
	k.kubelet(originalOf("q"), 100)
	if _, err := g.Reserve(context.Background(), "q", Protected, 100, 100, "/"+originalOf("q")); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("growth past live room: %v", err)
	}
}

func TestLiveBudgetContractionFencesViaReconcile(t *testing.T) {
	k := newModel()
	g, _, probe := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 1000, 100)
	reserveFor(t, g, k, "p", Protected, 300, 300)
	k.charge(be, 600, true)
	k.autoKill = true
	probe.set(func(b *Budget, _ *error) { b.Essential = 500 })
	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 300 || !k.wasKilled() {
		t.Fatal("live contraction not fenced")
	}
	onlyAggregateKills(t, k)
}

func TestAvailableBoundKeepsKubeletEvictionThreshold(t *testing.T) {
	k := newModel()
	g, _, probe := openFor(t, k)
	probe.set(func(b *Budget, _ *error) { b.Available, b.BestEffortResident, b.ProtectedResident = 500, 200, 100 })
	reserveFor(t, g, k, "p", Protected, 300, 300)
	// min(1000-300, 200 + (500 - 100 - (300-100))) = 400.
	if got := k.number(aggregate + "/memory.max"); got != 400 {
		t.Fatalf("live available bound %d", got)
	}
	k.kubelet(originalOf("q"), 450)
	if _, err := g.Reserve(context.Background(), "q", Protected, 450, 450, "/"+originalOf("q")); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("growth beyond killable best-effort plus available: %v", err)
	}
	reserveFor(t, g, k, "r", Protected, 400, 400)
	// min(1000-700, 200 + (500 - 100 - (700-100))) = 200.
	if got := k.number(aggregate + "/memory.max"); got != 200 {
		t.Fatalf("available bound after growth %d", got)
	}
}

func TestBestEffortReleaseRemovesUnpopulatedDescendantsWithResidue(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	be := reserveFor(t, g, k, "be", BestEffort, 600, 100)
	k.create(be + "/shim")
	k.charge(be+"/shim", 30, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := g.Release(ctx, "be"); !errors.Is(err, ErrHeld) {
		t.Fatalf("populated runtime descendant released: %v", err)
	}
	k.charge(be+"/shim", 30, false)
	if err := g.Release(context.Background(), "be"); err != nil {
		t.Fatal(err)
	}
	if k.has(be) || k.has(be+"/shim") || k.current(aggregate) != 30 {
		t.Fatal("unpopulated subtree not removed or residue not kept in aggregate")
	}
	if contract, _ := g.Contract("be"); contract.State != StateReleased {
		t.Fatal("best-effort release not tombstoned")
	}
}

func TestUnknownStateAndIdentityFailClosed(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	reserveFor(t, g, k, "p", Protected, 300, 300)
	k.kubelet(podRoot+"/pods", 100)
	for _, test := range []struct {
		id              string
		tier            Tier
		envelope, floor uint64
		parent          string
	}{
		{"p", BestEffort, 300, 300, "/" + originalOf("p")}, {"p", Protected, 300, 300, "/" + originalOf("other")},
		{"zero", Protected, 0, 0, "/" + originalOf("zero")}, {"overflow", Protected, math.MaxUint64, 1, "/" + originalOf("overflow")},
		{"floor", BestEffort, 100, 101, "/" + originalOf("floor")}, {"escape", Protected, 100, 100, "../escape"},
		{"systemd", Protected, 100, 100, "slice:prefix:name"}, {"ancestor", Protected, 100, 100, "/" + podRoot + "/pods"},
		{"aggregate", Protected, 100, 100, "/" + aggregate + "/x"},
	} {
		_, err := g.Reserve(context.Background(), test.id, test.tier, test.envelope, test.floor, test.parent)
		expectError(t, err)
	}
	if len(g.Contracts()) != 1 {
		t.Fatal("invalid reservation persisted")
	}
	k.readFault[aggregate+"/memory.current"] = "max"
	_, err := g.Reserve(context.Background(), "p", Protected, 300, 300, "/"+originalOf("p"))
	expectError(t, err)
}

func TestFailedAggregateLimitWriteOrReadbackNeverAdmits(t *testing.T) {
	for _, mode := range []string{"write", "readback"} {
		t.Run(mode, func(t *testing.T) {
			k := newModel()
			g, _, _ := openFor(t, k)
			if mode == "write" {
				k.writeFault[aggregate+"/memory.max"] = errors.New("kernel rejected")
			} else {
				k.readFault[aggregate+"/memory.max"] = "1000"
			}
			k.kubelet(originalOf("p"), 300)
			_, err := g.Reserve(context.Background(), "p", Protected, 300, 300, "/"+originalOf("p"))
			expectError(t, err)
			if _, ok := g.Contract("p"); ok || k.has(privateOf(Protected, "p")) {
				t.Fatal("protected cgroup delegated on aggregate write/readback failure")
			}
		})
	}
}

func TestRestartRejectsForeignStateAndRepairsOwnedLimits(t *testing.T) {
	for _, mode := range []string{"foreign", "corrupt", "repair"} {
		t.Run(mode, func(t *testing.T) {
			k := newModel()
			g, c, _ := openFor(t, k)
			be := reserveFor(t, g, k, "be", BestEffort, 600, 100)
			if err := g.Close(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "foreign":
				k.create(aggregate + "/foreign")
			case "corrupt":
				if err := os.WriteFile(c.StateDir+"/"+stateName, []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			case "repair":
				k.charge(be, 1, true)
				k.setFile(be, "memory.max", "max")
				k.setFile(be, "memory.swap.max", "1")
			}
			recovered, err := Open(c)
			if mode != "repair" {
				expectError(t, err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if k.file(be, "memory.max") != "600" || k.file(be, "memory.swap.max") != "0" || k.file(be, "memory.oom.group") != "1" {
				t.Fatal("restart did not reinstall owned best-effort limits")
			}
		})
	}
}

func TestRealNodeRebootTombstonesPriorBootContracts(t *testing.T) {
	k := newModel()
	g, c, probe := openFor(t, k)
	reserveFor(t, g, k, "old-protected", Protected, 800, 800)
	reserveFor(t, g, k, "old-be", BestEffort, 600, 100)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := newModel()
	c.Kernel, c.BootID = fresh, "test-boot-two"
	calls := probe.count()
	next, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	for _, contract := range next.Contracts() {
		if contract.State != StateReleased || contract.Boot != "test-boot-one" {
			t.Fatalf("prior-boot contract not tombstoned: %+v", contract)
		}
	}
	if len(next.Contracts()) != 2 || fresh.number(aggregate+"/memory.max") != 0 || probe.count() != calls {
		t.Fatal("new boot retained credit or needed a budget before any live contract")
	}
	reserveFor(t, next, fresh, "new-protected", Protected, 950, 950)
	if fresh.number(aggregate+"/memory.max") != 50 {
		t.Fatal("new boot did not derive room from its actual live contracts")
	}
	fresh.kubelet(originalOf("old-be"), 600)
	_, err = next.Reserve(context.Background(), "old-be", BestEffort, 600, 100, "/"+originalOf("old-be"))
	expectError(t, err)
	if err = next.Forget(context.Background(), "old-protected"); err != nil {
		t.Fatal(err)
	}
	if _, ok := next.Contract("old-protected"); ok {
		t.Fatal("tombstone not forgotten")
	}
}

func TestRealNodeRebootDoesNotAdoptForeignPopulatedAggregate(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	reserveFor(t, g, k, "old", Protected, 300, 300)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	fresh := newModel()
	fresh.create(aggregate)
	fresh.charge(aggregate, 1, true)
	c.Kernel, c.BootID = fresh, "test-boot-two"
	_, err := Open(c)
	expectError(t, err)
	if r := diskRecords(t, c, "test-boot-one")["old"]; r.State != StateAdmitted || r.Envelope != 300 {
		t.Fatal("failed new-boot adoption erased durable ownership")
	}
}

func TestBootstrapAndUnavailableBudgetDoNotBlockOpen(t *testing.T) {
	k := newModel()
	c, probe := configFor(t, k)
	probe.set(func(_ *Budget, err *error) { *err = errors.New("kubelet configuration not rendered") })
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	if probe.count() != 0 || k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("bootstrap needed a budget or granted room")
	}
	probe.set(func(_ *Budget, err *error) { *err = nil })
	reserveFor(t, g, k, "p", Protected, 300, 300)
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	probe.set(func(_ *Budget, err *error) { *err = errors.New("budget unavailable") })
	g, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if k.number(aggregate+"/memory.max") != 700 {
		t.Fatal("unavailable budget changed the finite aggregate bound")
	}
	k.kubelet(originalOf("q"), 100)
	_, err = g.Reserve(context.Background(), "q", Protected, 100, 100, "/"+originalOf("q"))
	expectError(t, err)
}

func TestForgetOnlyAfterRelease(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	if err := g.Forget(context.Background(), "unknown"); err != nil {
		t.Fatal(err)
	}
	reserveFor(t, g, k, "p", Protected, 300, 300)
	expectError(t, g.Forget(context.Background(), "p"))
	if err := g.Release(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if err := g.Release(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Fence(context.Background(), "p"); err == nil {
		t.Fatal("released contract fenced")
	}
	if err := g.Forget(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
}
