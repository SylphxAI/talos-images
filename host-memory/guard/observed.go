package guard

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SuspendBestEffort revokes BE room when mandatory observation is unavailable.
// fence reads charged bytes after reclaim/kill; issuing either is never credit.
func (g *Guard) SuspendBestEffort(ctx context.Context) error {
	g.observationFailed.Store(true)
	ctx, cancel, err := g.enter(ctx)
	if err != nil {
		return err
	}
	defer g.leave(cancel)
	g.observationFailed.Store(true)
	if err = g.kernel.Validate(ctx); err != nil {
		return err
	}
	if err = g.validateTree(ctx); err != nil {
		return err
	}
	return g.fence(ctx, 0, 0)
}

func (g *Guard) hasObserved() bool {
	for _, r := range g.store.records {
		if r.State != StateReleased && (r.Mode == Observed || r.NextMode == Observed) {
			return true
		}
	}
	return false
}

// Observed protected creation does not require a fictitious future reservation.
// If its live budget cannot be read, BE is revoked before delegation instead.
func (g *Guard) reconcileProtected(ctx context.Context, r record) error {
	if err := g.reconcile(ctx); err != nil {
		if r.Mode != Observed {
			return err
		}
		if stopErr := g.fence(ctx, 0, 0); stopErr != nil {
			return errors.Join(err, stopErr)
		}
	}
	return nil
}

func (g *Guard) checkOriginal(ctx context.Context, r record, mode MemoryMode) error {
	identity, limit, err := g.inspectOriginal(ctx, r.Original)
	if err != nil {
		return err
	}
	if identity != r.OriginalID {
		return errors.New("host-memory: kubelet pod cgroup replaced")
	}
	if (mode == Observed) != (limit == 0) {
		return errors.New("host-memory: original memory mode changed; authoritative pod update required")
	}
	return nil
}

// applyRecord never changes kubelet's original caps. For observed mode the
// private child must stay unlimited; only an explicit durable transition may
// remove a cap previously installed by this runtime.
func (g *Guard) applyRecord(ctx context.Context, r record, envelope, floor uint64) error {
	if r.Mode != Observed {
		return g.applyPrivate(ctx, r.Tier, r.Parent, envelope, floor)
	}
	if err := g.checkOriginal(ctx, r, Observed); err != nil {
		return err
	}
	for _, name := range []string{"memory.high", "memory.max"} {
		value, err := g.kernel.Read(ctx, r.Parent+"/"+name)
		if err != nil {
			return err
		}
		if strings.TrimSpace(value) != "max" {
			return fmt.Errorf("host-memory: observed private %s unexpectedly capped", name)
		}
	}
	return nil
}

// changeMode records intent before modifying a private cap. The old finite
// reservation remains counted until its cap is actually removed. Transition
// to finite counts the new full envelope before its private cap is installed.
func (g *Guard) changeMode(ctx context.Context, old record, mode MemoryMode, envelope, floor uint64) (Contract, error) {
	next := old
	next.NextMode, next.Intent, next.IntentFloor = mode, envelope, floor
	if mode == Finite {
		current, err := g.number(ctx, old.Original+"/memory.current")
		if err != nil {
			return Contract{}, err
		}
		if current > envelope {
			return Contract{}, errors.New("host-memory: finite transition still charged above enforced envelope")
		}
		if _, _, err = g.bounds(ctx, with(g.store.records, next), g.store.records); err != nil {
			return Contract{}, err
		}
	}
	if err := g.store.put(next); err != nil {
		return Contract{}, err
	}
	if err := g.settleMode(ctx, next); err != nil {
		return Contract{}, err
	}
	if err := g.reconcileProtected(ctx, g.store.records[old.ID]); err != nil {
		return Contract{}, err
	}
	return g.contract(old.ID), nil
}

// settleMode is replayable after every durable/kernel boundary. A changed
// original or private identity fails closed, never grants speculative credit.
func (g *Guard) settleMode(ctx context.Context, r record) error {
	present, _, _, err := g.observe(ctx, r)
	if err != nil {
		return err
	}
	if !present {
		return g.markReleased(r)
	}
	if err = g.checkOriginal(ctx, r, r.NextMode); err != nil {
		return err
	}
	next := r
	if r.NextMode == Observed {
		// Only our own private high/max are removed. The original is already max.
		for _, name := range []string{"memory.high", "memory.max"} {
			if err = g.kernel.Write(ctx, r.Parent+"/"+name, "max"); err != nil {
				return err
			}
			value, readErr := g.kernel.Read(ctx, r.Parent+"/"+name)
			if readErr != nil {
				return readErr
			}
			if strings.TrimSpace(value) != "max" {
				return errors.New("host-memory: failed to remove finite private cap")
			}
		}
		next.Envelope, next.Floor = 0, 0
	} else {
		// The original already enforces this cap across every sibling and residue.
		_, limit, readErr := g.inspectOriginal(ctx, r.Original)
		if readErr != nil {
			return readErr
		}
		if !g.pageEqual(limit, r.Intent) {
			return errors.New("host-memory: pending finite cap no longer matches original")
		}
		current, readErr := g.number(ctx, r.Original+"/memory.current")
		if readErr != nil {
			return readErr
		}
		if current > r.Intent {
			return errors.New("host-memory: finite transition charges exceed original cap")
		}
		if err = g.reconcile(ctx); err != nil {
			return err
		}
		if err = g.applyPrivate(ctx, Protected, r.Parent, r.Intent, r.IntentFloor); err != nil {
			return err
		}
		current, readErr = g.number(ctx, r.Original+"/memory.current")
		if readErr != nil {
			return readErr
		}
		if current > r.Intent {
			return errors.New("host-memory: finite transition charges not proven")
		}
		next.Envelope, next.Floor = r.Intent, r.IntentFloor
	}
	if err = g.checkOriginal(ctx, r, r.NextMode); err != nil {
		return err
	}
	identity, err := g.kernel.Identity(ctx, r.Parent)
	if err != nil {
		return err
	}
	if identity != r.ParentID {
		return errors.New("host-memory: private cgroup replaced during mode transition")
	}
	next.Mode, next.NextMode, next.Intent, next.IntentFloor = r.NextMode, "", 0, 0
	return g.store.put(next)
}

// ResumeBestEffort is called only after the runtime has installed and checked
// its observer. Room still requires fresh kernel/budget observations; failures
// latch admission closed and do not turn a successful watcher open into credit.
func (g *Guard) ResumeBestEffort(ctx context.Context) error {
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
	g.observationFailed.Store(false)
	if err = g.reconcile(ctx); err != nil {
		g.observationFailed.Store(true)
		cleanup, stop := g.cleanup(ctx)
		defer stop()
		return errors.Join(err, g.fence(cleanup, 0, 0))
	}
	return nil
}
