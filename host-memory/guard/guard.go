// Package guard is the single durable authority for native runtime memory
// contracts. It fences guest allocation with cgroup-v2 enforcement before the
// runtime creates or grows any process. Envelopes and floors include overhead.
package guard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MemoryMode separates a hard future envelope from uncapped observed usage.
// Observed does not reserve a request-sized maximum or intercept page faults.
type MemoryMode string

const (
	Finite   MemoryMode = "finite"
	Observed MemoryMode = "observed-protected"
)

// ProtectedOriginal is a deduplicated pod accounting boundary, not a class allowlist.
type ProtectedOriginal struct {
	Path string
	Mode MemoryMode
}

type Tier string

const (
	Protected  Tier = "protected"
	BestEffort Tier = "best-effort"
)

const (
	aggregate        = "sylphx-besteffort"
	bestEffortPrefix = "pod-"
	protectedPrefix  = "native-"
)

// State is the durable contract lifecycle:
//   - Pending: first admission intent is durable and counted as credit, but the
//     private cgroup was never returned to the runtime.
//   - Admitted: the private cgroup exists with the enforced Envelope.
//   - Releasing: release committed; credit held until the private cgroup is
//     observed unpopulated (or removed by the kernel owner) and removed.
//   - Released: tombstone without credit, kept until runtime metadata is
//     deleted and the caller invokes Forget.
type State string

const (
	StatePending   State = "pending"
	StateAdmitted  State = "admitted"
	StateReleasing State = "releasing"
	StateReleased  State = "released"
)

var (
	// ErrHeld is retryable: a release is still populated or the kernel refused
	// removal. Credit stays held; the caller retries later.
	ErrHeld = errors.New("host-memory: release held by populated cgroup")
	// ErrNoRoom rejects protected growth before any durable or kernel change.
	ErrNoRoom = errors.New("host-memory: protected reservations exceed live room")
)

// Budget is one live node observation in bytes. Physical, Allocatable,
// Essential and Eviction are the exactly-once node bounds. Available is the
// minimum MemAvailable across bracketing reads; ProtectedResident is the
// minimum observed sum of finite protected originals only; ObservedResident
// separately reports uncapped protected use without offsetting future credit;
// BestEffortResident is the minimum observed best-effort aggregate current.
type Budget struct {
	Physical, Allocatable, Essential, Eviction       uint64
	Available, ProtectedResident, BestEffortResident uint64
	ObservedResident                                 uint64
}

// Contract is a typed snapshot. Envelope/Floor are admitted and enforced (zero
// while Pending). Original is the kubelet pod parent; Parent is the private
// runtime parent. Boot is the kernel boot_id in which the contract was made.
type Contract struct {
	ID       string
	Tier     Tier
	Mode     MemoryMode
	State    State
	Envelope uint64
	Floor    uint64
	Original string
	Parent   string
	Boot     string
}

// Kernel is the actual I/O boundary, not an admission-policy substitute. Paths
// are validated, relative to CgroupRoot. Missing paths return errors matching
// os.ErrNotExist. Implementations respect context cancellation; WaitUnpopulated
// observes population events, never freed-byte credit. Production uses the
// Linux kernel with nonblocking limit writes and an owned, cancellable one-shot
// reclaim subprocess (never an in-process stuck write).
type Kernel interface {
	Lock() (io.Closer, error)
	Validate(context.Context) error
	Exists(context.Context, string) (bool, error)
	Identity(context.Context, string) (string, error)
	Mkdir(context.Context, string) error
	Children(context.Context, string) ([]string, error)
	Read(context.Context, string) (string, error)
	Write(context.Context, string, string) error
	Reclaim(context.Context, string, uint64) error
	WaitUnpopulated(context.Context, string) error
	Remove(context.Context, string) error
}

// Config.NodeBudget receives the sorted, deduplicated absolute kubelet pod
// parents and explicit modes of all protected contracts. It is called while the Guard
// holds its node lock and must never call back into the Guard.
type Config struct {
	CgroupRoot       string
	PodRoot          string
	StateDir         string
	NodeBudget       func(protectedOriginals []ProtectedOriginal) (Budget, error)
	BootID           string
	Kernel           Kernel
	OperationTimeout time.Duration
	ReclaimTimeout   time.Duration
}

type Guard struct {
	config            Config
	podRoot           string
	kernel            Kernel
	lock              io.Closer
	store             *stateStore
	serial            chan struct{}
	closed            bool
	observationFailed atomic.Bool
	view              atomic.Pointer[map[string]Contract]
	pageSize          uint64
}

// errPopulated is the held release case whose wait may happen outside the lock.
var errPopulated = fmt.Errorf("%w: private cgroup populated", ErrHeld)

// Open takes an exclusive node-wide cgroup-root lock for the Guard lifetime and
// reconstructs every contract from durable Guard state and kernel observation
// alone; it never re-admits intent. Ownership/integrity faults are errors.
// Held releases, missing kubelet cgroups and an unavailable live budget are
// not: they keep conservative credit and a finite aggregate hard bound.
func Open(c Config) (_ *Guard, err error) {
	if c.NodeBudget == nil {
		return nil, errors.New("host-memory: live NodeBudget required")
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = 5 * time.Second
	}
	if c.ReclaimTimeout == 0 {
		c.ReclaimTimeout = 500 * time.Millisecond
	}
	if c.OperationTimeout <= 0 || c.ReclaimTimeout <= 0 || c.ReclaimTimeout >= c.OperationTimeout {
		return nil, errors.New("host-memory: reclaim timeout must be positive and shorter than operation timeout")
	}
	if !canonicalAbs(c.CgroupRoot) || !canonicalAbs(c.StateDir) {
		return nil, errors.New("host-memory: canonical absolute cgroup and state directories required")
	}
	podRoot, err := cleanParent(c.PodRoot)
	if err != nil {
		return nil, fmt.Errorf("host-memory: kubelet pod root: %w", err)
	}
	if c.BootID == "" {
		if c.BootID, err = kernelBootID(); err != nil {
			return nil, err
		}
	} else if !validBootID(c.BootID) {
		return nil, errors.New("host-memory: invalid kernel boot identity")
	}
	k := c.Kernel
	if k == nil {
		if k, err = newLinuxKernel(c.CgroupRoot); err != nil {
			return nil, err
		}
	}
	lock, err := k.Lock()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), c.OperationTimeout)
	defer cancel()
	if err = k.Validate(ctx); err != nil {
		return nil, err
	}
	s, err := openState(c.StateDir, c.CgroupRoot, podRoot, c.BootID)
	if err != nil {
		return nil, err
	}
	g := &Guard{config: c, podRoot: podRoot, kernel: k, lock: lock, store: s, serial: make(chan struct{}, 1), pageSize: uint64(os.Getpagesize())}
	g.serial <- struct{}{}
	exists, err := k.Exists(ctx, aggregate)
	if err != nil {
		return nil, err
	}
	if exists && !s.existing {
		return nil, errors.New("host-memory: aggregate exists without durable ownership")
	}
	if !s.existing {
		// Ownership is durable before the aggregate is created.
		if err = s.commit(s.records, ""); err != nil {
			return nil, err
		}
	}
	if !exists {
		if err = k.Mkdir(ctx, aggregate); err != nil {
			return nil, err
		}
	}
	identity, err := k.Identity(ctx, aggregate)
	if err != nil {
		return nil, err
	}
	if identity == "" {
		return nil, errors.New("host-memory: unknown aggregate identity")
	}
	if s.aggregateID == "" || s.priorBoot {
		if err = g.assertAdoptable(ctx); err != nil {
			return nil, err
		}
		records := s.records
		if s.priorBoot {
			records = priorBootTombstones(s.records)
		}
		if err = s.commit(records, identity); err != nil {
			return nil, err
		}
		s.priorBoot = false
	} else if s.aggregateID != identity {
		return nil, errors.New("host-memory: aggregate cgroup ownership replaced")
	}
	if err = g.validateTree(ctx); err != nil {
		return nil, err
	}
	if err = g.enable(ctx, aggregate); err != nil {
		return nil, err
	}
	if err = g.settle(ctx, true); err != nil {
		if g.hasObserved() {
			cleanup, stop := g.cleanup(ctx)
			err = errors.Join(err, g.fence(cleanup, 0, 0))
			stop()
		}
		return nil, err
	}
	if s.legacy {
		if err = s.commit(s.records, identity); err != nil {
			return nil, err
		}
	}
	if g.live() {
		if fenceErr := g.reconcile(ctx); fenceErr != nil {
			hold, holdCancel := context.WithTimeout(context.Background(), c.ReclaimTimeout)
			err = g.holdAggregate(hold)
			holdCancel()
			if err != nil {
				return nil, errors.Join(fenceErr, err)
			}
		}
	} else if err = g.setAggregate(ctx, 0, 0); err != nil {
		// Bootstrap without live contracts grants no allocation and needs no
		// live budget, so image APIs work before kubelet accounting exists.
		return nil, err
	}
	g.publish()
	return g, nil
}

// Reserve admits a new contract or applies an authoritative pod-level update.
// The kubelet original must exist with a finite memory.max; a new or changed
// envelope must equal it (page rounding safe). Protected growth is checked
// against live room before any durable write (ErrNoRoom), recorded as intent,
// fenced by shrinking/reclaiming/killing best-effort only, and only then is the
// private cap raised. Failure before cap growth rolls the intent back; an
// existing admitted contract stays usable. A protected shrink returns credit
// only after the kubelet original already enforces it and its charges fit.
// An unchanged request is an idempotent re-fence without admission.
func (g *Guard) Reserve(ctx context.Context, id string, tier Tier, envelope, floor uint64, originalParent string) (Contract, error) {
	return g.reserve(ctx, id, tier, envelope, floor, originalParent, false)
}

// Protect selects finite versus observed from the actual kubelet memory.max,
// never from requests or CRI's possibly partial sum of limited containers.
func (g *Guard) Protect(ctx context.Context, id string, envelope, floor uint64, originalParent string) (Contract, error) {
	return g.reserve(ctx, id, Protected, envelope, floor, originalParent, true)
}

func (g *Guard) reserve(ctx context.Context, id string, tier Tier, envelope, floor uint64, originalParent string, allowObserved bool) (Contract, error) {
	ctx, cancel, err := g.enter(ctx)
	if err != nil {
		return Contract{}, err
	}
	defer g.leave(cancel)
	checkEnvelope, checkFloor := envelope, floor
	if allowObserved && envelope == 0 && floor == 0 {
		checkEnvelope, checkFloor = 1, 1
	}
	if err = validateRequest(id, tier, checkEnvelope, checkFloor); err != nil {
		return Contract{}, err
	}
	original, err := g.original(originalParent)
	if err != nil {
		return Contract{}, err
	}
	if err = g.kernel.Validate(ctx); err != nil {
		return Contract{}, err
	}
	if err = g.settle(ctx, false); err != nil {
		return Contract{}, err
	}
	if tier == BestEffort && g.observationFailed.Load() {
		return Contract{}, errors.New("host-memory: mandatory observation unavailable")
	}
	old, exists := g.store.records[id]
	if exists {
		if old.Tier != tier || old.Original != original {
			return Contract{}, errors.New("host-memory: contract identity changed")
		}
		if old.State != StateAdmitted {
			return Contract{}, fmt.Errorf("host-memory: contract %s is %s, not admitted", id, old.State)
		}
	}
	originalID, limit, err := g.inspectOriginal(ctx, original)
	if err != nil {
		return Contract{}, err
	}
	if exists && tier == Protected && old.OriginalID != originalID {
		return Contract{}, errors.New("host-memory: kubelet pod cgroup replaced")
	}
	if limit == 0 {
		if !allowObserved || tier != Protected {
			return Contract{}, errors.New("host-memory: finite kubelet pod memory.max required")
		}
		if err := g.changeOriginalMode(ctx, original, originalID, Observed, 0, 0); err != nil {
			return Contract{}, err
		}
		if exists {
			return g.refence(ctx, g.store.records[id])
		}
		return g.admit(ctx, record{ID: id, Tier: Protected, Mode: Observed, State: StatePending, Original: original, OriginalID: originalID, Parent: privateParent(Protected, id, original), Boot: g.config.BootID})
	}
	if envelope == 0 || floor == 0 {
		return Contract{}, errors.New("host-memory: finite original requires full positive envelope")
	}
	if exists && old.Mode == Observed {
		if !g.pageEqual(limit, envelope) {
			return Contract{}, errors.New("host-memory: finite original differs from envelope")
		}
		return g.changeMode(ctx, old, Finite, envelope, floor)
	}
	changed := !exists || old.Envelope != envelope || old.Floor != floor
	if changed && !g.pageEqual(limit, envelope) {
		return Contract{}, fmt.Errorf("host-memory: kubelet pod memory.max %d is not the full contract envelope %d", limit, envelope)
	}
	switch {
	case !exists:
		return g.admit(ctx, record{ID: id, Tier: tier, Mode: Finite, State: StatePending, Original: original, OriginalID: originalID, Parent: privateParent(tier, id, original), Intent: envelope, IntentFloor: floor, Boot: g.config.BootID})
	case !changed:
		return g.refence(ctx, old)
	case tier == BestEffort:
		return g.resizeBestEffort(ctx, old, envelope, floor)
	case envelope <= old.Envelope:
		return g.shrinkProtected(ctx, old, envelope, floor)
	default:
		return g.growProtected(ctx, old, envelope, floor)
	}
}

// Fence re-establishes an admitted contract before a runtime allocation
// (container create/start/task) without admission or growth. A missing or
// replaced private cgroup is kernel proof the sandbox emptied: it settles to
// Released and Fence fails.
func (g *Guard) Fence(ctx context.Context, id string) (Contract, error) {
	ctx, cancel, err := g.enter(ctx)
	if err != nil {
		return Contract{}, err
	}
	defer g.leave(cancel)
	if err = g.kernel.Validate(ctx); err != nil {
		return Contract{}, err
	}
	if err = g.settle(ctx, false); err != nil {
		return Contract{}, err
	}
	r, ok := g.store.records[id]
	if !ok || r.State != StateAdmitted {
		return Contract{}, fmt.Errorf("host-memory: contract %s is not admitted", id)
	}
	if r.Tier == BestEffort && g.observationFailed.Load() {
		return Contract{}, errors.New("host-memory: mandatory observation unavailable")
	}
	return g.refence(ctx, r)
}

// Release commits release, then removes the unpopulated private subtree. It
// never kills and never requires zero charges: residual page cache reparents
// into the aggregate or kubelet original, which remain measured. A populated
// subtree is waited for outside the node lock (bounded by OperationTimeout),
// then revalidated; if still populated, errors.Is(err, ErrHeld). nil means the
// contract is a Released tombstone (or unknown).
func (g *Guard) Release(ctx context.Context, id string) error {
	if ctx == nil {
		return errors.New("host-memory: nil context")
	}
	ctx, cancel := context.WithTimeout(ctx, g.config.OperationTimeout)
	defer cancel()
	for {
		if err := g.lockNode(ctx); err != nil {
			return err
		}
		wait, err := g.releaseLocked(ctx, id)
		g.unlockNode()
		if err != nil || wait == "" {
			return err
		}
		if err = g.kernel.WaitUnpopulated(ctx, wait); err != nil && !errors.Is(err, os.ErrNotExist) {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %v", ErrHeld, err)
			}
			return err
		}
	}
}

// Forget removes a Released tombstone after the runtime deleted its metadata.
func (g *Guard) Forget(ctx context.Context, id string) error {
	ctx, cancel, err := g.enter(ctx)
	if err != nil {
		return err
	}
	defer g.leave(cancel)
	r, ok := g.store.records[id]
	if !ok {
		return nil
	}
	if r.State != StateReleased {
		return fmt.Errorf("host-memory: cannot forget %s contract %s", r.State, id)
	}
	return g.store.remove(id)
}

// Reconcile settles orphaned contracts, recomputes the live best-effort bound
// and fences it. The native PSI watcher calls it on kernel pressure events; it
// supplements, never replaces, the aggregate hard boundary.
func (g *Guard) Reconcile(ctx context.Context) error {
	ctx, cancel, err := g.enter(ctx)
	if err != nil {
		return err
	}
	defer g.leave(cancel)
	if err = g.kernel.Validate(ctx); err != nil {
		return err
	}
	if err = g.validateTree(ctx); err != nil {
		return err
	}
	if err = g.settle(ctx, false); err != nil {
		return err
	}
	return g.reconcile(ctx)
}

// Contract returns a lock-free snapshot of one contract.
func (g *Guard) Contract(id string) (Contract, bool) {
	view := g.view.Load()
	if view == nil {
		return Contract{}, false
	}
	c, ok := (*view)[id]
	return c, ok
}

// Contracts returns a lock-free snapshot of all contracts sorted by ID.
func (g *Guard) Contracts() []Contract {
	view := g.view.Load()
	if view == nil {
		return nil
	}
	out := make([]Contract, 0, len(*view))
	for _, c := range *view {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (g *Guard) Close() error {
	<-g.serial
	defer func() { g.serial <- struct{}{} }()
	if g.closed {
		return nil
	}
	g.closed = true
	return g.lock.Close()
}

// UsableRoom implements the exactly-once accounting contract. Essential and
// eviction are subtracted only from physical memory, never from allocatable.
func UsableRoom(b Budget) (uint64, error) {
	if b.Physical == 0 || b.Allocatable == 0 || b.Allocatable > b.Physical || b.Physical > math.MaxInt64 ||
		b.Available > b.Physical || b.ProtectedResident > b.Physical || b.ObservedResident > b.Physical || b.BestEffortResident > b.Physical {
		return 0, errors.New("host-memory: unknown/inconsistent node budget")
	}
	deduction, err := add(b.Essential, b.Eviction)
	if err != nil {
		return 0, err
	}
	return min(b.Allocatable, sub(b.Physical, deduction)), nil
}

func add(a, b uint64) (uint64, error) {
	if math.MaxUint64-a < b {
		return 0, errors.New("host-memory: byte sum overflow")
	}
	return a + b, nil
}

func sub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}

func (g *Guard) enter(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, errors.New("host-memory: nil context")
	}
	bounded, cancel := context.WithTimeout(ctx, g.config.OperationTimeout)
	if err := g.lockNode(bounded); err != nil {
		cancel()
		return nil, nil, err
	}
	return bounded, cancel, nil
}

func (g *Guard) leave(cancel context.CancelFunc) { g.unlockNode(); cancel() }

func (g *Guard) lockNode(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.serial:
	}
	if err := ctx.Err(); err != nil {
		g.serial <- struct{}{}
		return err
	}
	if g.closed {
		g.serial <- struct{}{}
		return errors.New("host-memory: guard closed")
	}
	return nil
}

func (g *Guard) unlockNode() { g.publish(); g.serial <- struct{}{} }

func (g *Guard) publish() {
	view := make(map[string]Contract, len(g.store.records))
	for id, r := range g.store.records {
		view[id] = Contract{ID: r.ID, Tier: r.Tier, Mode: r.Mode, State: r.State, Envelope: r.Envelope, Floor: r.Floor, Original: "/" + r.Original, Parent: "/" + r.Parent, Boot: r.Boot}
	}
	g.view.Store(&view)
}

func (g *Guard) contract(id string) Contract {
	r := g.store.records[id]
	return Contract{ID: r.ID, Tier: r.Tier, Mode: r.Mode, State: r.State, Envelope: r.Envelope, Floor: r.Floor, Original: "/" + r.Original, Parent: "/" + r.Parent, Boot: r.Boot}
}

// cleanup bounds rollback I/O even when the caller's context was cancelled.
func (g *Guard) cleanup(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), g.config.ReclaimTimeout)
}

func (g *Guard) admit(ctx context.Context, r record) (Contract, error) {
	candidate := with(g.store.records, r)
	if err := validateOverlap(candidate); err != nil {
		return Contract{}, err
	}
	if r.Tier == Protected && r.Mode != Observed {
		if _, _, err := g.bounds(ctx, candidate, g.store.records); err != nil {
			return Contract{}, err
		}
	}
	// Durable intent precedes every kernel change, so a crash leaves a
	// counted, recoverable Pending record and never an unowned cgroup.
	if err := g.store.put(r); err != nil {
		return Contract{}, err
	}
	if err := g.install(ctx, r); err != nil {
		cleanup, cancel := g.cleanup(ctx)
		if rollbackErr := g.rollbackPending(cleanup, g.store.records[r.ID]); rollbackErr != nil {
			err = errors.Join(err, rollbackErr)
		}
		cancel()
		if ctx.Err() == nil {
			_ = g.reconcile(ctx)
		}
		return Contract{}, err
	}
	return g.contract(r.ID), nil
}

func (g *Guard) install(ctx context.Context, r record) error {
	if err := g.validateTree(ctx); err != nil {
		return err
	}
	if r.Tier == Protected {
		// Shrink/reclaim/kill best-effort to the bound that already includes
		// this intent BEFORE any protected cgroup can be delegated.
		if err := g.reconcileProtected(ctx, r); err != nil {
			return err
		}
		if err := g.enable(ctx, r.Original); err != nil {
			return err
		}
	}
	if err := g.kernel.Mkdir(ctx, r.Parent); err != nil {
		return err
	}
	if err := g.applyRecord(ctx, r, r.Intent, r.IntentFloor); err != nil {
		return err
	}
	if err := g.enable(ctx, r.Parent); err != nil {
		return err
	}
	identity, err := g.kernel.Identity(ctx, r.Parent)
	if err != nil {
		return err
	}
	if identity == "" {
		return errors.New("host-memory: unknown private cgroup identity")
	}
	if r.Tier == BestEffort {
		if err = g.reconcile(ctx); err != nil {
			return err
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	next := r
	next.State, next.Envelope, next.Floor, next.Intent, next.IntentFloor, next.ParentID = StateAdmitted, r.Intent, r.IntentFloor, 0, 0, identity
	return g.store.put(next)
}

func (g *Guard) refence(ctx context.Context, r record) (Contract, error) {
	present, _, _, err := g.observe(ctx, r)
	if err != nil {
		return Contract{}, err
	}
	if !present {
		if err = g.markReleased(r); err != nil {
			return Contract{}, err
		}
		return Contract{}, fmt.Errorf("host-memory: contract %s private cgroup emptied by kernel", r.ID)
	}
	if err = g.applyRecord(ctx, r, r.Envelope, r.Floor); err != nil {
		return Contract{}, err
	}
	if err = g.reconcileProtected(ctx, r); err != nil {
		return Contract{}, err
	}
	if r.Mode == Observed {
		if err = g.checkOriginal(ctx, r, Observed); err != nil {
			return Contract{}, err
		}
		identity, err := g.kernel.Identity(ctx, r.Parent)
		if err != nil {
			return Contract{}, err
		}
		if identity != r.ParentID {
			return Contract{}, errors.New("host-memory: private cgroup replaced during observed fence")
		}
	}
	return g.contract(r.ID), nil
}

// Best-effort envelopes grant no protected credit: the aggregate hard bound
// confines them, so the per-pod limits simply follow the authoritative update.
func (g *Guard) resizeBestEffort(ctx context.Context, old record, envelope, floor uint64) (Contract, error) {
	if err := g.applyPrivate(ctx, BestEffort, old.Parent, envelope, floor); err != nil {
		cleanup, cancel := g.cleanup(ctx)
		_ = g.applyPrivate(cleanup, BestEffort, old.Parent, old.Envelope, old.Floor)
		cancel()
		return Contract{}, err
	}
	next := old
	next.Envelope, next.Floor = envelope, floor
	if err := g.store.put(next); err != nil {
		return Contract{}, err
	}
	if err := g.reconcile(ctx); err != nil {
		return Contract{}, err
	}
	return g.contract(old.ID), nil
}

// The kubelet original already reads back the smaller envelope (verified by
// the caller), so the kernel enforces it across every holder and residue.
func (g *Guard) shrinkProtected(ctx context.Context, old record, envelope, floor uint64) (Contract, error) {
	charged, err := g.number(ctx, old.Original+"/memory.current")
	if err != nil {
		return Contract{}, err
	}
	if charged > envelope {
		return Contract{}, fmt.Errorf("host-memory: protected charges %d exceed shrunk envelope %d; credit retained", charged, envelope)
	}
	if err = g.applyPrivate(ctx, Protected, old.Parent, envelope, floor); err != nil {
		return Contract{}, err
	}
	next := old
	next.Envelope, next.Floor = envelope, floor
	if err = g.store.put(next); err != nil {
		return Contract{}, err
	}
	if err = g.reconcile(ctx); err != nil {
		return Contract{}, err
	}
	return g.contract(old.ID), nil
}

func (g *Guard) growProtected(ctx context.Context, old record, envelope, floor uint64) (Contract, error) {
	intent := old
	intent.Intent, intent.IntentFloor = envelope, floor
	if _, _, err := g.bounds(ctx, with(g.store.records, intent), g.store.records); err != nil {
		return Contract{}, err
	}
	if err := g.store.put(intent); err != nil {
		return Contract{}, err
	}
	capTouched := false
	fail := func(err error) (Contract, error) {
		cleanup, cancel := g.cleanup(ctx)
		defer cancel()
		var rollbackErr error
		if capTouched {
			rollbackErr = g.settleGrowth(cleanup, g.store.records[old.ID])
		} else {
			rollbackErr = g.store.put(old)
		}
		if ctx.Err() == nil {
			_ = g.reconcile(ctx)
		}
		return Contract{}, errors.Join(err, rollbackErr)
	}
	if err := g.validateTree(ctx); err != nil {
		return fail(err)
	}
	if err := g.reconcile(ctx); err != nil {
		return fail(err)
	}
	present, _, _, err := g.observe(ctx, old)
	if err != nil {
		return fail(err)
	}
	if !present {
		return fail(fmt.Errorf("host-memory: contract %s private cgroup emptied by kernel", old.ID))
	}
	capTouched = true
	if err = g.applyPrivate(ctx, Protected, old.Parent, envelope, floor); err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	next := old
	next.Envelope, next.Floor = envelope, floor
	if err = g.store.put(next); err != nil {
		return fail(err)
	}
	return g.contract(old.ID), nil
}

func (g *Guard) releaseLocked(ctx context.Context, id string) (string, error) {
	r, ok := g.store.records[id]
	if !ok || r.State == StateReleased {
		return "", nil
	}
	if r.State == StatePending {
		if err := g.rollbackPending(ctx, r); err != nil {
			return "", err
		}
		if r, ok = g.store.records[id]; !ok {
			return "", nil
		}
	}
	if r.State == StateAdmitted && r.Intent > 0 && r.NextMode == "" {
		if err := g.settleGrowth(ctx, r); err != nil {
			return "", err
		}
		r = g.store.records[id]
	}
	if r.State == StateAdmitted {
		r.State = StateReleasing
		if err := g.store.put(r); err != nil {
			return "", err
		}
	}
	if r.State == StateReleasing {
		err := g.finishRelease(ctx, r)
		if err == errPopulated {
			return r.Parent, nil
		}
		if err != nil {
			return "", err
		}
	}
	// The release is durable; expanding the best-effort bound is conservative
	// best effort and surfaces on the next Reserve/Fence/Reconcile.
	if ctx.Err() == nil {
		_ = g.reconcile(ctx)
	}
	return "", nil
}

// settle advances every contract from kernel observation without waiting:
// rolls back Pending intent, resolves interrupted growth, releases contracts
// whose private cgroup the kernel owner removed, and finishes unpopulated
// releases. Held releases are not errors.
func (g *Guard) settle(ctx context.Context, recovering bool) error {
	for _, r := range g.ordered() {
		var err error
		switch r.State {
		case StatePending:
			err = g.rollbackPending(ctx, r)
		case StateAdmitted:
			if r.NextMode != "" {
				if err = g.settleMode(ctx, r); err != nil {
					break
				}
				r = g.store.records[r.ID]
			}
			if r.Intent > 0 {
				if err = g.settleGrowth(ctx, r); err != nil {
					break
				}
				r = g.store.records[r.ID]
				if r.State != StateAdmitted {
					break
				}
			}
			var present bool
			present, _, _, err = g.observe(ctx, r)
			if err == nil && !present {
				err = g.markReleased(r)
			}
		case StateReleasing:
			if r.NextMode != "" {
				if err = g.settleMode(ctx, r); err != nil {
					break
				}
				r = g.store.records[r.ID]
				if r.State == StateReleased {
					break
				}
			}
			err = g.finishRelease(ctx, r)
		}
		if err != nil && !errors.Is(err, ErrHeld) {
			return err
		}
	}
	// Kubelet may have changed its actual cap before CRI persisted an update or
	// before the runtime crashed. Discover that authority before reinstating an
	// old private cap. Lifecycle settlement above removes absent holders first.
	seen := map[string]bool{}
	for _, r := range g.ordered() {
		if r.Tier != Protected || r.State != StateAdmitted || seen[r.Original] {
			continue
		}
		seen[r.Original] = true
		identity, limit, err := g.inspectOriginal(ctx, r.Original)
		if err != nil {
			return err
		}
		if identity != r.OriginalID {
			return errors.New("host-memory: kubelet pod cgroup replaced")
		}
		mode := Finite
		if limit == 0 {
			mode = Observed
		}
		if mode != r.Mode {
			if err = g.changeOriginalMode(ctx, r.Original, identity, mode, limit, limit); err != nil {
				return err
			}
		}
	}
	if recovering {
		for _, r := range g.ordered() {
			if r.State == StateAdmitted {
				if err := g.applyRecord(ctx, r, r.Envelope, r.Floor); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// observe reports whether the contract's own private cgroup still exists. A
// missing path or a different identity is kernel proof the old cgroup had no
// tasks and no live children when it was removed.
func (g *Guard) observe(ctx context.Context, r record) (present, populated bool, identity string, err error) {
	identity, err = g.kernel.Identity(ctx, r.Parent)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", err
	}
	if r.ParentID != "" && identity != r.ParentID {
		return false, false, "", nil
	}
	text, err := g.kernel.Read(ctx, r.Parent+"/cgroup.events")
	if errors.Is(err, os.ErrNotExist) {
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", err
	}
	populated, err = population(text)
	return err == nil, populated, identity, err
}

// rollbackPending undoes a first admission never returned to the runtime. A
// populated private cgroup (impossible without a returned contract) is held
// conservatively as admitted (its credit was already counted) and its private
// limits are reinstalled rather than trusted.
func (g *Guard) rollbackPending(ctx context.Context, r record) error {
	present, populated, identity, err := g.observe(ctx, r)
	if err != nil {
		return err
	}
	if present && populated {
		next := r
		next.State, next.Envelope, next.Floor, next.Intent, next.IntentFloor, next.ParentID = StateAdmitted, r.Intent, r.IntentFloor, 0, 0, identity
		if err = g.store.put(next); err != nil {
			return err
		}
		return g.applyRecord(ctx, next, next.Envelope, next.Floor)
	}
	if present {
		if err = g.removeTree(ctx, r.Parent); err != nil {
			return fmt.Errorf("%w: %v", ErrHeld, err)
		}
	}
	return g.store.remove(r.ID)
}

// settleGrowth resolves an interrupted protected growth without re-admitting
// it. If the larger cap was not committed (or is observed restored with the
// charges inside the old envelope), the intent rolls back; otherwise the
// conservative larger envelope stays admitted until an observed safe shrink.
func (g *Guard) settleGrowth(ctx context.Context, r record) error {
	present, _, _, err := g.observe(ctx, r)
	if err != nil {
		return err
	}
	if !present {
		return g.markReleased(r)
	}
	limit, err := g.number(ctx, r.Parent+"/memory.max")
	if err != nil {
		return err
	}
	if limit > r.Envelope {
		// Lower the uncommitted cap only when the observed charges already fit;
		// never force reclaim/OOM on a protected guest to recover credit.
		charged, chargeErr := g.number(ctx, r.Parent+"/memory.current")
		if chargeErr == nil && charged <= r.Envelope && g.applyPrivate(ctx, Protected, r.Parent, r.Envelope, r.Floor) == nil {
			if charged, chargeErr = g.number(ctx, r.Parent+"/memory.current"); chargeErr == nil && charged <= r.Envelope {
				limit = r.Envelope
			} else {
				_ = g.applyPrivate(ctx, Protected, r.Parent, r.Intent, r.IntentFloor)
			}
		}
	}
	next := r
	if limit > r.Envelope {
		next.Envelope, next.Floor = r.Intent, r.IntentFloor
	} else {
		// The cap never grew; reinstate the matching high (a partial write may
		// have raised it). Failure is harmless: max still enforces the envelope.
		_ = g.applyPrivate(ctx, Protected, r.Parent, r.Envelope, r.Floor)
	}
	next.Intent, next.IntentFloor = 0, 0
	return g.store.put(next)
}

func (g *Guard) finishRelease(ctx context.Context, r record) error {
	present, populated, _, err := g.observe(ctx, r)
	if err != nil {
		return err
	}
	if present && populated {
		return errPopulated
	}
	if present {
		if err = g.removeTree(ctx, r.Parent); err != nil {
			return fmt.Errorf("%w: %v", ErrHeld, err)
		}
	}
	return g.markReleased(r)
}

func (g *Guard) markReleased(r record) error {
	r.State, r.Intent, r.IntentFloor, r.NextMode = StateReleased, 0, 0, ""
	return g.store.put(r)
}

// removeTree removes an unpopulated runtime-owned subtree bottom-up. The kernel
// refuses rmdir of a populated cgroup; residual charges reparent upward.
func (g *Guard) removeTree(ctx context.Context, parent string) error {
	children, err := g.kernel.Children(ctx, parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, child := range children {
		if child == "" || path.Base(child) != child || child == "." || child == ".." {
			return errors.New("host-memory: unsafe descendant name")
		}
		if err = g.removeTree(ctx, parent+"/"+child); err != nil {
			return err
		}
	}
	if err = g.kernel.Remove(ctx, parent); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (g *Guard) live() bool {
	for _, r := range g.store.records {
		if r.State != StateReleased {
			return true
		}
	}
	return false
}

func (g *Guard) ordered() []record {
	ids := make([]string, 0, len(g.store.records))
	for id := range g.store.records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]record, 0, len(ids))
	for _, id := range ids {
		out = append(out, g.store.records[id])
	}
	return out
}

func with(records map[string]record, r record) map[string]record {
	out := make(map[string]record, len(records)+1)
	for key, value := range records {
		out[key] = value
	}
	out[r.ID] = r
	return out
}

func priorBootTombstones(records map[string]record) map[string]record {
	out := make(map[string]record, len(records))
	for id, r := range records {
		r.State, r.Intent, r.IntentFloor, r.NextMode = StateReleased, 0, 0, ""
		out[id] = r
	}
	return out
}

// tally returns holder-max protected credit per kubelet original (multiple
// sandboxes of one pod share one original envelope) and best-effort floors.
func tally(records map[string]record) (map[string]uint64, uint64, error) {
	groups := make(map[string]uint64)
	var floors uint64
	var err error
	for _, r := range records {
		if r.State == StateReleased {
			continue
		}
		if r.Tier == Protected {
			groups[r.Original] = max(groups[r.Original], r.credit())
		} else if floors, err = add(floors, max(r.Floor, r.IntentFloor)); err != nil {
			return nil, 0, err
		}
	}
	return groups, floors, nil
}

func total(groups map[string]uint64) (uint64, error) {
	var sum uint64
	var err error
	for _, credit := range groups {
		if sum, err = add(sum, credit); err != nil {
			return 0, err
		}
	}
	return sum, nil
}

// bounds derives the best-effort hard bound and high from one live budget.
// Exact-once: room = min(A, P-H-E); each original counts max(holder credit,
// its observed charges) once. Supplemental live bound keeps MemAvailable above
// the kubelet eviction threshold: BEResident + (Available - Eviction -
// unconsumed protected reservations). When before is non-nil and protected
// credit grows, admission must fit both bounds or fail with ErrNoRoom.
func (g *Guard) bounds(ctx context.Context, records, before map[string]record) (uint64, uint64, error) {
	groups, floors, err := tally(records)
	if err != nil {
		return 0, 0, err
	}
	keys := make([]string, 0, len(groups))
	for original := range groups {
		keys = append(keys, original)
	}
	sort.Strings(keys)
	originals := make([]ProtectedOriginal, len(keys))
	for i, original := range keys {
		mode := Observed
		for _, r := range records {
			if r.State != StateReleased && r.Tier == Protected && r.Original == original && (r.Mode != Observed || r.NextMode == Finite) {
				mode = Finite
			}
		}
		originals[i] = ProtectedOriginal{Path: "/" + original, Mode: mode}
	}
	b, err := g.config.NodeBudget(originals)
	if err != nil {
		return 0, 0, err
	}
	room, err := UsableRoom(b)
	if err != nil {
		return 0, 0, err
	}
	reserved, err := total(groups)
	if err != nil {
		return 0, 0, err
	}
	var charged uint64
	for _, original := range keys {
		current, readErr := g.number(ctx, original+"/memory.current")
		if errors.Is(readErr, os.ErrNotExist) {
			current = 0
		} else if readErr != nil {
			return 0, 0, readErr
		}
		if charged, err = add(charged, max(groups[original], current)); err != nil {
			return 0, 0, err
		}
	}
	liveRoom := func(reservations uint64) (uint64, error) {
		deduction, err := add(b.Eviction, sub(reservations, b.ProtectedResident))
		if err != nil {
			return 0, err
		}
		return add(b.BestEffortResident, sub(b.Available, deduction))
	}
	if before != nil {
		previousGroups, _, err := tally(before)
		if err != nil {
			return 0, 0, err
		}
		previous, err := total(previousGroups)
		if err != nil {
			return 0, 0, err
		}
		if reserved > previous {
			if charged > room {
				return 0, 0, fmt.Errorf("%w: %d > %d", ErrNoRoom, charged, room)
			}
			available, err := liveRoom(previous)
			if err != nil {
				return 0, 0, err
			}
			if reserved-previous > available {
				return 0, 0, fmt.Errorf("%w: growth %d exceeds live available %d", ErrNoRoom, reserved-previous, available)
			}
		}
	}
	live, err := liveRoom(reserved)
	if err != nil {
		return 0, 0, err
	}
	bound := min(sub(room, charged), live)
	if g.observationFailed.Load() {
		bound = 0
	}
	return bound, min(bound, floors), nil
}

func (g *Guard) reconcile(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		bound, high, err := g.bounds(ctx, g.store.records, nil)
		if err != nil {
			return err
		}
		if err = g.fence(ctx, bound, high); err != nil {
			return err
		}
		// Re-observe after potentially delayed reclaim/kill; no stale budget
		// observation may grant protected execution when room contracted.
		latest, _, err := g.bounds(ctx, g.store.records, nil)
		if err != nil {
			return err
		}
		actual, err := g.number(ctx, aggregate+"/memory.max")
		if err != nil {
			return err
		}
		if actual <= latest {
			return ctx.Err()
		}
	}
}

// fence installs the aggregate hard bound, then requires observed charges at
// or below it: bounded reclaim, then cgroup.kill of the best-effort aggregate
// only. After a kill it waits a bounded interval for the population event,
// re-kills racing shims, reclaims residual cache once unpopulated, and judges
// actual current <= bound, never zero. Writes and kills are never credited.
func (g *Guard) fence(ctx context.Context, bound, high uint64) error {
	if err := g.setAggregate(ctx, high, bound); err != nil {
		return err
	}
	limit, err := g.number(ctx, aggregate+"/memory.max")
	if err != nil {
		return err
	}
	within := func() (bool, uint64, error) {
		current, err := g.number(ctx, aggregate+"/memory.current")
		return current <= limit, current, err
	}
	ok, current, err := within()
	if err != nil || ok {
		return err
	}
	g.reclaim(ctx, current-limit)
	if ok, current, err = within(); err != nil || ok {
		return err
	}
	for {
		if err = g.kernel.Write(ctx, aggregate+"/cgroup.kill", "1"); err != nil {
			return err
		}
		wait, cancel := context.WithTimeout(ctx, g.config.ReclaimTimeout)
		waitErr := g.kernel.WaitUnpopulated(wait, aggregate)
		cancel()
		if err = ctx.Err(); err != nil {
			return err
		}
		if ok, current, err = within(); err != nil || ok {
			return err
		}
		if waitErr == nil {
			// No process can charge more; only residual cache/kernel memory.
			g.reclaim(ctx, current-limit)
			if ok, current, err = within(); err != nil || ok {
				return err
			}
			return fmt.Errorf("host-memory: best-effort residue %d remains above fence %d", current, limit)
		}
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			return waitErr
		}
		// Still populated after a bounded wait: a racing shim joined. It is
		// confined by the installed hard max; kill again.
	}
}

func (g *Guard) reclaim(ctx context.Context, amount uint64) {
	bounded, cancel := context.WithTimeout(ctx, g.config.ReclaimTimeout)
	_ = g.kernel.Reclaim(bounded, aggregate, amount)
	cancel()
}

// holdAggregate keeps the aggregate on a finite hard bound when the live
// budget cannot be read or fenced: an existing finite bound is kept, an
// unbounded one becomes zero.
func (g *Guard) holdAggregate(ctx context.Context) error {
	if g.hasObserved() {
		return g.fence(ctx, 0, 0)
	}
	limit, err := g.number(ctx, aggregate+"/memory.max")
	if err != nil {
		limit = 0
	}
	high, err := g.number(ctx, aggregate+"/memory.high")
	if err != nil {
		high = 0
	}
	return g.setAggregate(ctx, min(high, limit), limit)
}

type setting struct {
	name  string
	value uint64
}

// Aggregate: no swap, no group OOM (one best-effort pod fails, not every pod).
func (g *Guard) setAggregate(ctx context.Context, high, bound uint64) error {
	return g.setLimits(ctx, aggregate, []setting{{"memory.swap.max", 0}, {"memory.oom.group", 0}, {"memory.high", high}, {"memory.max", bound}})
}

// Private children: best-effort pods get swap off and per-pod group OOM;
// protected children get only the finite envelope (high=max), never swap or
// OOM-group changes that would widen protected failure.
func (g *Guard) applyPrivate(ctx context.Context, tier Tier, parent string, envelope, floor uint64) error {
	if tier == BestEffort {
		return g.setLimits(ctx, parent, []setting{{"memory.swap.max", 0}, {"memory.oom.group", 1}, {"memory.high", floor}, {"memory.max", envelope}})
	}
	return g.setLimits(ctx, parent, []setting{{"memory.high", envelope}, {"memory.max", envelope}})
}

func (g *Guard) setLimits(ctx context.Context, parent string, settings []setting) error {
	// O_NONBLOCK memory.max/high prevents an unbounded in-process reclaim syscall.
	for _, s := range settings {
		p := parent + "/" + s.name
		if err := g.kernel.Write(ctx, p, strconv.FormatUint(s.value, 10)); err != nil {
			return err
		}
		actual, err := g.number(ctx, p)
		if err != nil {
			return err
		}
		if s.name == "memory.oom.group" || s.name == "memory.swap.max" {
			if actual != s.value {
				return fmt.Errorf("host-memory: %s readback mismatch", p)
			}
		} else if actual > s.value {
			return fmt.Errorf("host-memory: %s exceeds intended bound", p)
		}
	}
	high, err := g.number(ctx, parent+"/memory.high")
	if err != nil {
		return err
	}
	bound, err := g.number(ctx, parent+"/memory.max")
	if err != nil {
		return err
	}
	if high > bound {
		return errors.New("host-memory: high exceeds hard max")
	}
	return nil
}

func (g *Guard) assertAdoptable(ctx context.Context) error {
	children, err := g.kernel.Children(ctx, aggregate)
	if err != nil {
		return err
	}
	text, err := g.kernel.Read(ctx, aggregate+"/cgroup.events")
	if err != nil {
		return err
	}
	populated, err := population(text)
	if err != nil {
		return err
	}
	current, err := g.number(ctx, aggregate+"/memory.current")
	if err != nil {
		return err
	}
	if len(children) != 0 || populated || current != 0 {
		return errors.New("host-memory: refusing to adopt a used unbound aggregate")
	}
	return nil
}

func (g *Guard) validateTree(ctx context.Context) error {
	children, err := g.kernel.Children(ctx, aggregate)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool)
	for _, r := range g.store.records {
		if r.Tier == BestEffort && r.State != StateReleased {
			allowed[path.Base(r.Parent)] = true
		}
	}
	for _, child := range children {
		if !allowed[child] {
			return fmt.Errorf("host-memory: unowned best-effort child %q", child)
		}
	}
	return nil
}

// inspectOriginal reads the kubelet-owned pod cgroup without writing it.
func (g *Guard) inspectOriginal(ctx context.Context, original string) (string, uint64, error) {
	identity, err := g.kernel.Identity(ctx, original)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, fmt.Errorf("host-memory: kubelet pod cgroup /%s does not exist", original)
	}
	if err != nil {
		return "", 0, err
	}
	if identity == "" {
		return "", 0, errors.New("host-memory: unknown kubelet pod cgroup identity")
	}
	text, err := g.kernel.Read(ctx, original+"/memory.max")
	if err != nil {
		return "", 0, err
	}
	if strings.TrimSpace(text) == "max" {
		return identity, 0, nil
	}
	limit, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil || limit == 0 || limit > math.MaxInt64 {
		return "", 0, fmt.Errorf("host-memory: unknown kubelet pod memory.max %q", strings.TrimSpace(text))
	}
	return identity, limit, nil
}

// The kernel stores memory.max in pages, rounding the written byte value down.
func (g *Guard) pageEqual(limit, envelope uint64) bool {
	return limit <= envelope && envelope-limit < g.pageSize
}

func (g *Guard) original(p string) (string, error) {
	rel, err := cleanParent(p)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(rel, g.podRoot+"/") {
		return "", errors.New("host-memory: original pod parent outside kubelet pod root")
	}
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, protectedPrefix) {
			return "", errors.New("host-memory: runtime-private cgroup is not a kubelet pod parent")
		}
	}
	return rel, nil
}

// enable delegates the memory controller to children of a domain cgroup. On a
// kubelet original this only enables the controller, as stock runc does when
// it creates container cgroups; its limits are never written.
func (g *Guard) enable(ctx context.Context, parent string) error {
	kind, err := g.kernel.Read(ctx, parent+"/cgroup.type")
	if err != nil {
		return err
	}
	if strings.TrimSpace(kind) != "domain" {
		return errors.New("host-memory: non-domain cgroup")
	}
	controllers, err := g.kernel.Read(ctx, parent+"/cgroup.controllers")
	if err != nil {
		return err
	}
	if !word(controllers, "memory") {
		return errors.New("host-memory: memory controller unavailable")
	}
	enabled, err := g.kernel.Read(ctx, parent+"/cgroup.subtree_control")
	if err != nil {
		return err
	}
	if !word(enabled, "memory") {
		if err = g.kernel.Write(ctx, parent+"/cgroup.subtree_control", "+memory"); err != nil {
			return err
		}
		if enabled, err = g.kernel.Read(ctx, parent+"/cgroup.subtree_control"); err != nil {
			return err
		}
		if !word(enabled, "memory") {
			return errors.New("host-memory: memory controller delegation readback failed")
		}
	}
	return nil
}

func privateParent(tier Tier, id, original string) string {
	digest := sha256.Sum256([]byte(id))
	name := hex.EncodeToString(digest[:])
	if tier == BestEffort {
		return aggregate + "/" + bestEffortPrefix + name
	}
	return original + "/" + protectedPrefix + name
}

func ownedName(name, prefix string) bool {
	digest, ok := strings.CutPrefix(name, prefix)
	if !ok || len(digest) != 2*sha256.Size {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ownedPrivate reports whether p is a runtime-private cgroup root
// (aggregate/pod-<sha> or <original>/native-<sha>) or a descendant of one.
func ownedPrivate(p string) bool {
	parts := strings.Split(p, "/")
	if len(parts) >= 2 && parts[0] == aggregate && ownedName(parts[1], bestEffortPrefix) {
		return true
	}
	for i, part := range parts {
		if i > 0 && ownedName(part, protectedPrefix) {
			return true
		}
	}
	return false
}

func canonicalAbs(p string) bool { return filepath.IsAbs(p) && filepath.Clean(p) == p }

func cleanParent(p string) (string, error) {
	if p == "" || strings.Contains(p, "\x00") || strings.Contains(p, ":") {
		return "", errors.New("host-memory: pod filesystem parent required")
	}
	rel := strings.TrimPrefix(p, "/")
	if rel == "" || path.Clean(rel) != rel || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errors.New("host-memory: unsafe pod parent")
	}
	if rel == aggregate || strings.HasPrefix(rel, aggregate+"/") {
		return "", errors.New("host-memory: original parent overlaps owned best-effort subtree")
	}
	return rel, nil
}

func validateRequest(id string, tier Tier, envelope, floor uint64) error {
	if id == "" || len(id) > 512 || strings.Contains(id, "\x00") || (tier != Protected && tier != BestEffort) ||
		envelope == 0 || envelope > math.MaxInt64 || floor == 0 || floor > envelope {
		return errors.New("host-memory: invalid/unbounded reservation")
	}
	return nil
}

func word(s, w string) bool {
	for _, v := range strings.Fields(s) {
		if v == w {
			return true
		}
	}
	return false
}

func (g *Guard) number(ctx context.Context, p string) (uint64, error) {
	text, err := g.kernel.Read(ctx, p)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil || n > math.MaxInt64 {
		return 0, fmt.Errorf("host-memory: unknown/nonfinite %s", p)
	}
	return n, nil
}

func population(text string) (bool, error) {
	found, value := false, false
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return false, errors.New("host-memory: malformed cgroup.events")
		}
		if fields[0] == "populated" {
			if found || (fields[1] != "0" && fields[1] != "1") {
				return false, errors.New("host-memory: invalid population state")
			}
			found, value = true, fields[1] == "1"
		}
	}
	if !found {
		return false, errors.New("host-memory: missing population state")
	}
	return value, nil
}
