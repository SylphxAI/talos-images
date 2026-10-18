package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func observeFor(t *testing.T, g *Guard, k *modelKernel, id string) Contract {
	t.Helper()
	k.kubelet(originalOf(id), 100)
	k.setFile(originalOf(id), "memory.max", "max")
	c, err := g.Protect(context.Background(), id, 0, 0, "/"+originalOf(id))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != Observed || c.Envelope != 0 || c.Floor != 0 {
		t.Fatalf("invented envelope: %+v", c)
	}
	return c
}
func TestObservedProtectedIsNotARequestSizedEnvelope(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	observed := observeFor(t, g, k, "agent")
	k.charge(originalOf("agent"), 350, true) // > hypothetical request100, no cap
	if _, err := g.Fence(context.Background(), observed.ID); err != nil {
		t.Fatal(err)
	}
	if got := k.file(observed.Parent[1:], "memory.max"); got != "max" {
		t.Fatalf("protected capped at %s", got)
	}
	if got := k.number(aggregate + "/memory.max"); got != 650 {
		t.Fatalf("expected actual350 deducted once from1000, got%d", got)
	}
	if err := g.SuspendBestEffort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := k.number(aggregate + "/memory.max"); got != 0 {
		t.Fatalf("suspension granted%d", got)
	}
	if k.file(originalOf("agent"), "memory.max") != "max" {
		t.Fatal("original cap changed")
	}
}
func TestObservedDoesNotCancelOtherPodsFiniteFutureReservation(t *testing.T) {
	k := newModel()
	g, _, b := openFor(t, k)
	reserveAt(t, g, k, "finite", Protected, 300, 300, originalOf("finite"))
	observeFor(t, g, k, "observed")
	k.charge(originalOf("finite"), 100, true)
	k.charge(originalOf("observed"), 350, true)
	b.set(func(b *Budget, _ *error) { b.Available = 500; b.ProtectedResident = 100; b.ObservedResident = 350 })
	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	// live BE room500 - eviction100 - finite unconsumed200 =200.
	if got := k.number(aggregate + "/memory.max"); got != 200 {
		t.Fatalf("observed usage cancelled finite credit: %d", got)
	}
}
func TestProtectedModeTransitionsRemoveOnlyOwnedCapAndRequireChargedProof(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	reserveAt(t, g, k, "p", Protected, 400, 400, originalOf("p"))
	original := "/" + originalOf("p")
	k.setFile(originalOf("p"), "memory.max", "max")
	r, err := g.Protect(context.Background(), "p", 0, 0, original)
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != Observed || k.file(r.Parent[1:], "memory.max") != "max" || k.file(r.Parent[1:], "memory.high") != "max" {
		t.Fatal("stale private cap after transition")
	}
	k.charge(originalOf("p"), 500, true)
	k.setFile(originalOf("p"), "memory.max", "300")
	if _, err = g.Protect(context.Background(), "p", 300, 300, original); err == nil {
		t.Fatal("returned credit without charged-byte proof")
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("failed charged-byte proof left speculative BE room")
	}
	if r, _ = g.Contract("p"); r.Mode != Observed {
		t.Fatal("failed transition changed contract")
	}
	k.setFile(originalOf("p"), "memory.max", "600")
	r, err = g.Protect(context.Background(), "p", 600, 600, original)
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != Finite || r.Envelope != 600 || k.number(r.Parent[1:]+"/memory.max") != 600 {
		t.Fatal("finite transition not enforced")
	}
}
func TestObservedTransitionSurvivesInterruptedCapRemoval(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	reserveAt(t, g, k, "p", Protected, 400, 400, originalOf("p"))
	parent := privateParent(Protected, "p", originalOf("p"))
	k.setFile(originalOf("p"), "memory.max", "max")
	k.writeFault[parent+"/memory.max"] = errors.New("write interrupted")
	if _, err = g.Protect(context.Background(), "p", 0, 0, "/"+originalOf("p")); err == nil {
		t.Fatal("fault ignored")
	}
	if g.store.records["p"].NextMode != Observed {
		t.Fatal("transition intent lost")
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	delete(k.writeFault, parent+"/memory.max")
	g, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	r, _ := g.Contract("p")
	if r.Mode != Observed || k.file(parent, "memory.max") != "max" {
		t.Fatal("restart restored stale finite cap")
	}
}
func TestObservedOriginalReplacementNeverAdoptsNewIdentity(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	r := observeFor(t, g, k, "p")
	k.mu.Lock()
	k.groups[originalOf("p")].id++
	k.mu.Unlock()
	if _, err := g.Fence(context.Background(), r.ID); err == nil {
		t.Fatal("replacement identity adopted")
	}
}
func TestObservedBudgetFailureRevokesBEWithoutCappingProtected(t *testing.T) {
	k := newModel()
	g, _, b := openFor(t, k)
	observeFor(t, g, k, "p")
	b.set(func(_ *Budget, err *error) { *err = errors.New("physical counters unreadable") })
	if _, err := g.Fence(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("BE kept stale room")
	}
}
func TestObservedBurstDoesNotClaimSynchronousAllocationBarrier(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	observeFor(t, g, k, "p")
	reserveAt(t, g, k, "vm", BestEffort, 900, 900, originalOf("vm"))
	k.charge(privateParent(BestEffort, "vm", originalOf("vm")), 700, true)
	k.charge(originalOf("p"), 400, true)
	// The original is uncapped: charged usage can grow before an async event.
	// No synchronous guarantee is asserted; the installed BE bound is still1000.
	if k.number(aggregate+"/memory.max") != 1000 {
		t.Fatal("unexpected pre-event bound")
	}
	if err := g.Reconcile(context.Background()); err == nil {
		t.Fatal("stalled reclaim/kill returned speculative credit")
	}
	if got := k.number(aggregate + "/memory.max"); got != 600 {
		t.Fatalf("new bound not enforced:%d", got)
	}
	if !k.wasKilled() {
		t.Fatal("stalled reclaim did not target BE")
	}
	if k.file(originalOf("p"), "memory.max") != "max" {
		t.Fatal("protected original capped")
	}
}
func TestTypedStateLegacyMigrationAndDowngradeRefusal(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	reserveAt(t, g, k, "p", Protected, 100, 100, originalOf("p"))
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(c.StateDir, stateName)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	if err = json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	disk["version"] = float64(3)
	for _, v := range disk["reservations"].([]any) {
		delete(v.(map[string]any), "memory_mode")
	}
	data, _ = json.Marshal(disk)
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	g, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := g.Contract("p")
	if r.Mode != Finite {
		t.Fatal("legacy not finite")
	}
	g.Close()
	// v3 readers reject v4 and unknown memory_mode fields, rather than silently
	// treating observed zero bytes as a finite envelope. Downgrade requires drain.
	data, _ = os.ReadFile(file)
	json.Unmarshal(data, &disk)
	if disk["version"] != float64(4) {
		t.Fatal("legacy not rewritten as typed state")
	}
}

func TestObservedIgnoresPartialCRILimitsAndNeverTreatsThemAsFutureMax(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	k.kubelet(originalOf("p"), 100)
	k.setFile(originalOf("p"), "memory.max", "max")
	c, err := g.Protect(context.Background(), "p", 256, 256, "/"+originalOf("p"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != Observed || c.Envelope != 0 {
		t.Fatal("partial CRI sum became protected cap")
	}
}
func TestObserverSuspensionCannotBeReopenedByProtectedReconcile(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	observeFor(t, g, k, "p")
	if err := g.SuspendBestEffort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Fence(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("protected fence reopened BE while observer dead")
	}
	k.kubelet(originalOf("be"), 100)
	if _, err := g.Reserve(context.Background(), "be", BestEffort, 100, 100, "/"+originalOf("be")); err == nil {
		t.Fatal("BE admitted while observer dead")
	}
	if err := g.ResumeBestEffort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 1000 {
		t.Fatal("fresh observation did not restore actual room")
	}
}

func TestReleaseCanFinishAnInterruptedModeChangeWithoutRewritingProtectedCaps(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	reserveAt(t, g, k, "p", Protected, 400, 400, originalOf("p"))
	parent := privateParent(Protected, "p", originalOf("p"))
	k.setFile(originalOf("p"), "memory.max", "max")
	k.writeFault[parent+"/memory.max"] = errors.New("interrupted")
	if _, err := g.Protect(context.Background(), "p", 0, 0, "/"+originalOf("p")); err == nil {
		t.Fatal("fault ignored")
	}
	if err := g.Release(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	c, _ := g.Contract("p")
	if c.State != StateReleased {
		t.Fatal("transition prevented normal stop")
	}
	if k.file(originalOf("p"), "memory.max") != "max" {
		t.Fatal("release changed original")
	}
}
func TestObservedPhysicalCounterMustBeConsistent(t *testing.T) {
	_, err := UsableRoom(Budget{Physical: 100, Allocatable: 100, ObservedResident: 101})
	if err == nil {
		t.Fatal("impossible observed physical use accepted")
	}
}

func TestSharedOriginalModeChangeCoversEverySandboxHolder(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	original := originalOf("pod")
	k.kubelet(original, 400)
	for _, id := range []string{"a", "b"} {
		if _, err := g.Reserve(context.Background(), id, Protected, 400, 400, "/"+original); err != nil {
			t.Fatal(err)
		}
	}
	k.setFile(original, "memory.max", "max")
	if _, err := g.Protect(context.Background(), "c", 0, 0, "/"+original); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		r, _ := g.Contract(id)
		if r.Mode != Observed || r.Envelope != 0 || k.file(r.Parent[1:], "memory.max") != "max" {
			t.Fatalf("mixed holder survived:%+v", r)
		}
	}
	k.charge(privateParent(Protected, "a", original), 100, true)
	k.charge(privateParent(Protected, "b", original), 150, true)
	k.charge(privateParent(Protected, "c", original), 200, true)
	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if k.number(aggregate+"/memory.max") != 550 {
		t.Fatal("shared observed charges not counted exactly once")
	}
	k.setFile(original, "memory.max", "600")
	if _, err := g.Protect(context.Background(), "c", 600, 600, "/"+original); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		r, _ := g.Contract(id)
		if r.Mode != Finite || r.Envelope != 600 || k.number(r.Parent[1:]+"/memory.max") != 600 {
			t.Fatalf("holder not finite:%+v", r)
		}
	}
	if k.number(aggregate+"/memory.max") != 400 {
		t.Fatal("shared finite envelope counted more than once")
	}
}
func TestSharedOriginalModeIntentIsDurableBeforeFirstPrivateCapChange(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	original := originalOf("pod")
	k.kubelet(original, 400)
	for _, id := range []string{"a", "b"} {
		if _, err = g.Reserve(context.Background(), id, Protected, 400, 400, "/"+original); err != nil {
			t.Fatal(err)
		}
	}
	k.setFile(original, "memory.max", "max")
	second := privateParent(Protected, "b", original)
	k.writeFault[second+"/memory.max"] = errors.New("second holder interrupted")
	if _, err = g.Protect(context.Background(), "a", 0, 0, "/"+original); err == nil {
		t.Fatal("fault ignored")
	}
	if g.store.records["a"].Mode != Observed || g.store.records["b"].NextMode != Observed {
		t.Fatal("shared intent not durable")
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	delete(k.writeFault, second+"/memory.max")
	g, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, id := range []string{"a", "b"} {
		r, _ := g.Contract(id)
		if r.Mode != Observed || k.file(r.Parent[1:], "memory.max") != "max" {
			t.Fatal("restart retained mixed mode", r)
		}
	}
}

func TestInterruptedModeEnforcementRevokesBEWithoutTouchingProtectedOriginal(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	reserveAt(t, g, k, "p", Protected, 400, 400, originalOf("p"))
	vm := reserveAt(t, g, k, "vm", BestEffort, 900, 900, originalOf("vm"))
	k.charge(vm.Parent[1:], 500, true)
	k.autoKill = true
	k.setFile(originalOf("p"), "memory.max", "max")
	k.writeFault[privateOf(Protected, "p")+"/memory.max"] = errors.New("mode write failed")
	if _, err := g.Protect(context.Background(), "p", 0, 0, "/"+originalOf("p")); err == nil {
		t.Fatal("mode fault ignored")
	}
	if k.number(aggregate+"/memory.max") != 0 || !k.wasKilled() {
		t.Fatal("unresolved mode enforcement left BE room")
	}
	if k.file(originalOf("p"), "memory.max") != "max" {
		t.Fatal("protected original changed")
	}
}

func TestRestartDiscoversUncappedOriginalBeforeRestoringOldPrivateCap(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	original := originalOf("pod")
	k.kubelet(original, 400)
	for _, id := range []string{"a", "b"} {
		if _, err = g.Reserve(context.Background(), id, Protected, 400, 400, "/"+original); err != nil {
			t.Fatal(err)
		}
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	// Crash after kubelet applied max, before any CRI update/typed intent.
	k.setFile(original, "memory.max", "max")
	g, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, id := range []string{"a", "b"} {
		r, _ := g.Contract(id)
		if r.Mode != Observed || k.file(r.Parent[1:], "memory.max") != "max" {
			t.Fatal("startup retained an obsolete private cap", r)
		}
	}
}
func TestFenceDiscoversMissedUncappedPodUpdate(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	reserveAt(t, g, k, "p", Protected, 400, 400, originalOf("p"))
	k.setFile(originalOf("p"), "memory.max", "max")
	r, err := g.Fence(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != Observed || k.file(r.Parent[1:], "memory.max") != "max" {
		t.Fatal("ordinary allocation kept stale private cap")
	}
}
func TestOwnerCanReverseInterruptedModeChangeThroughDurableProof(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	reserveAt(t, g, k, "p", Protected, 400, 400, originalOf("p"))
	parent := privateOf(Protected, "p")
	k.setFile(originalOf("p"), "memory.max", "max")
	k.writeFault[parent+"/memory.max"] = errors.New("interrupted")
	if _, err := g.Protect(context.Background(), "p", 0, 0, "/"+originalOf("p")); err == nil {
		t.Fatal("fault ignored")
	}
	delete(k.writeFault, parent+"/memory.max")
	k.setFile(originalOf("p"), "memory.max", "400")
	r, err := g.Fence(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if r.Mode != Finite || r.Envelope != 400 || k.number(parent+"/memory.high") != 400 || k.number(parent+"/memory.max") != 400 {
		t.Fatal("reversed owner cap not re-proven", r)
	}
}
func TestRestartCannotClaimFiniteRoomWhileObservedChargesExceedNewOriginalCap(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	observeFor(t, g, k, "p")
	k.charge(originalOf("p"), 500, true)
	g.Close()
	k.setFile(originalOf("p"), "memory.max", "300")
	if opened, err := Open(c); err == nil {
		opened.Close()
		t.Fatal("restart granted finite credit above charged-byte proof")
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("failed recovery left speculative BE room")
	}
}

func TestClearedModeIntentCannotWriteFromAnOlderHolderSnapshot(t *testing.T) {
	k := newModel()
	g, _, _ := openFor(t, k)
	r := observeFor(t, g, k, "p")
	parent := r.Parent[1:]
	k.setFile(originalOf("p"), "memory.max", "600")
	k.writeFault[parent+"/memory.max"] = errors.New("finite transition interrupted")
	if _, err := g.Protect(context.Background(), "p", 600, 600, "/"+originalOf("p")); err == nil {
		t.Fatal("fault ignored")
	}
	stale := g.store.records["p"]
	if stale.NextMode != Finite {
		t.Fatal("no persisted finite intent")
	}
	delete(k.writeFault, parent+"/memory.max")
	k.setFile(originalOf("p"), "memory.max", "max")
	if _, err := g.Fence(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	// The original later becomes finite again. Replaying the old snapshot must
	// not reuse its cleared authority: a fresh durable intent is required.
	k.setFile(originalOf("p"), "memory.max", "600")
	if err := g.settleMode(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	current := g.store.records["p"]
	if current.Mode != Observed || current.NextMode != "" || k.file(parent, "memory.max") != "max" {
		t.Fatal("stale intent wrote without durable authority", current)
	}
	if _, err := g.Fence(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	current = g.store.records["p"]
	if current.Mode != Finite || current.Envelope != 600 {
		t.Fatal("fresh authoritative discovery did not work", current)
	}
}

func TestPendingRecoveryValidatesOriginalIdentityBeforePrivateCapWrite(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	original := originalOf("p")
	k.kubelet(original, 300)
	parent := privateOf(Protected, "p")
	if err = g.store.put(record{ID: "p", Tier: Protected, State: StatePending, Original: original, OriginalID: "foreign", Parent: parent, Intent: 300, IntentFloor: 300, Boot: c.BootID}); err != nil {
		t.Fatal(err)
	}
	k.create(parent)
	k.charge(parent, 50, true)
	g.Close()
	if opened, err := Open(c); err == nil {
		opened.Close()
		t.Fatal("foreign pending identity adopted")
	}
	if k.file(parent, "memory.max") != "max" {
		t.Fatal("private cap written before original identity proof")
	}
	if k.number(aggregate+"/memory.max") != 0 {
		t.Fatal("failed recovery left BE room")
	}
}
func TestPendingRecoveryUsesTypedModeIntentBeforeRemovingAnObsoletePrivateCap(t *testing.T) {
	k := newModel()
	c, _ := configFor(t, k)
	g, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	original := originalOf("p")
	k.kubelet(original, 300)
	parent := privateOf(Protected, "p")
	identity, err := k.Identity(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if err = g.store.put(record{ID: "p", Tier: Protected, State: StatePending, Original: original, OriginalID: identity, Parent: parent, Intent: 300, IntentFloor: 300, Boot: c.BootID}); err != nil {
		t.Fatal(err)
	}
	k.create(parent)
	k.setFile(parent, "memory.max", "300")
	k.setFile(parent, "memory.high", "300")
	k.charge(parent, 50, true)
	g.Close()
	k.setFile(original, "memory.max", "max")
	g, err = Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	r, _ := g.Contract("p")
	if r.Mode != Observed || r.Envelope != 0 || k.file(parent, "memory.max") != "max" {
		t.Fatal("pending recovery kept stale private cap", r)
	}
}

func TestDrainedStateKeepsExactLegacyV3OwnershipWithoutDowngradingTombstones(t *testing.T) {
	k := newModel()
	g, c, _ := openFor(t, k)
	observeFor(t, g, k, "p")
	identity := g.store.aggregateID
	if err := g.Release(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(c.StateDir, stateName)
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var d diskState
	if err = json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != 4 || len(d.Records) != 1 || d.Records[0].State != StateReleased {
		t.Fatal("nonempty tombstone journal downgraded", d)
	}
	if err = g.Forget(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	expected := fmt.Sprintf("{\"version\":3,\"cgroup_root\":%q,\"pod_root\":%q,\"boot_id\":%q,\"aggregate_identity\":%q,\"reservations\":[]}\n", c.CgroupRoot, g.podRoot, c.BootID, identity)
	if string(data) != expected {
		t.Fatalf("empty state not exact legacy shape:%s", data)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.store.aggregateID != identity || len(recovered.Contracts()) != 0 {
		t.Fatal("empty rollback lost ownership")
	}
	observeFor(t, recovered, k, "next")
	data, err = os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != 4 || len(d.Records) != 1 || d.Records[0].Mode != Observed {
		t.Fatal("active observed contract used legacy format", d)
	}
}
