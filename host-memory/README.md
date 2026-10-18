# Native host-memory protection

Namespace/Depot-style managed microVM execution keeps runtime configuration and
placement in the platform. Public shape, TTL, region and smaller guest-base
overrides remain optional and tenant-neutral. Company workloads use the same
public lease, grants and limits; this mechanism adds no tenant exemption.

## Native runtime boundary

The protection boundary belongs in the native container runtime, before it
creates a shim or a guest, not in the Kubernetes writer or a post-start watcher.
The pinned containerd, Kata and kubelet integration in this directory builds into
the immutable Talos runtime stack. It is not a CRI executable wrapper, privileged
Pod, second scheduler or desk daemon.

Stock containerd 2.3.5 calls NRI `RunPodSandbox` after starting the sandbox; that
hook cannot pause before first-guest allocation. Its standalone sandbox-controller
proxy also needs recovery and resize-routing repairs. Stock Kata 3.32.0 moves
its shim into the pod cgroup before starting the VMM, but moving a process does
not move its existing memory charges. The native integration must therefore
place the long-lived shim at process creation, not claim that later migration
accounts its earlier heap. Sources:
[containerd sandbox start](https://github.com/containerd/containerd/blob/v2.3.5/internal/cri/server/sandbox_run.go),
[Kata host cgroups](https://github.com/kata-containers/kata-containers/blob/3.32.0/docs/design/host-cgroups.md),
[Linux memory ownership](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-ownership).

## Accounting and ordering

`guard` owns durable per-node reservations and the best-effort host hierarchy.
Tier comes from the reviewed runtime binding, never Kubernetes zero-request
BestEffort/Burstable QoS or tenant-supplied labels. The full-request accounting
handler is best-effort; other workloads retain protection. Unknown contracts cannot create speculative room. Protected contracts have an
explicit finite or observed mode, selected from the actual kubelet original
`memory.max`, never requests, labels or a partial CRI sum of container limits.

All values are bytes. Given live physical memory `P`, allocatable `A`, actual
essential host requirement `H` and the actual eviction requirement `E`:

```text
usable room = min(A, max(0, P - H - E))
best-effort hard bound = max(0, usable room - sum(max(finite envelope, actual charges)) - observed protected charges)
```

Allocatable already excludes reservations and eviction accounting. Taking the
minimum of the two independently derived bounds does not subtract those
reservations twice. Effective pod envelopes include guest memory and the exact
RuntimeClass/Pod overhead once; parent and child cgroup usage are not summed.
Replacement sandboxes may share one kubelet pod identity: its protected envelope
is reserved once, while each sandbox has a separate runtime-owned child. The
guard never creates or overwrites kubelet's original pod parent. A finite kernel limit
must agree with the effective pod envelope before admission. An unlimited
original selects observed protection: its actual charged bytes count exactly
once, but its request is never treated as a future maximum. The private child
retains `memory.high=max` and `memory.max=max`; original caps are never edited.
There is no fixed idle margin, queue reservation or private pool.

Before first guest allocation, the aggregate and per-sandbox host cgroups must
exist with the matching typed contract. Best-effort children require finite
`memory.high`/`memory.max`, disable
swap and use group OOM containment; protected settings do not widen kubelet's
kill policy. All BE VMM, shim, I/O processes and guest backing allocations belong
inside those bounds. A finite protected create or growth first shrinks the best-effort
bound, attempts reclaim, and observes actual charged bytes below that bound.
Insufficient or stalled reclaim escalates only within the best-effort subtree.
Writing a limit, freeing guest pages or issuing a kill is not evidence that
memory has returned. Protected execution cannot proceed on stale release credit.

One guard journal owns intents, admitted contracts and release tombstones.
Runtime restart retains these; a real kernel boot identity separately proves
that old processes cannot survive. A failed uncommitted admission cannot poison
the current contract. Container updates do not add growth already reserved by
the authoritative pod update.

Cleanup distinguishes occupancy from memory charges. Kernel removal proves a
subtree has no processes; remaining cache/slab charges are still accounted in
their aggregate or parent rather than credited as freed bytes. Released markers
survive until ordinary runtime metadata deletion finishes, making either crash
ordering recoverable. Unknown/unreserved pod residue remains host use. Unsupported
factory, remote or checkpoint paths fail closed, never bypassing the allocation barrier.


## Observed protection and failure boundaries

Uncapped protected allocation remains a normal kernel allocation, not a new CRI
request. Its future burst is unknown. PSI reports stalls asynchronously and the
kubelet reacts at eviction thresholds; neither provides a synchronous reservation
before every page fault. This runtime does not promise zero global host OOMs.
BE ancestry, finite VM/overhead limits, OOM adjustment and BE-first kubelet eviction
remain enforced. Acceptance must observe BE-only victims and protected continuity
under simultaneous anonymous growth and stalled reclaim on the supported worker.

Every BE allocation requires a live installed PSI observer and a fresh physical
budget read. Observer installation failure, destruction or reconciliation failure latches BE
closed and revokes the aggregate bound to zero. A healthy observer installation
after a runtime restart preserves recovered BE workloads and their freshly
reconciled bound. Reclaim and kill requests do not
free accounting credit: current charges are read back and an unsuccessful release
remains an error. Protected creation may proceed with BE revoked when accounting
is unavailable; it never obtains an invented finite reservation. Missing or
unowned residents remain essential host usage, rather than being excluded.

Finite and observed protected usage are sampled in the same bracketing physical
read. Only finite resident bytes offset finite future reservations in the
MemAvailable bound. A large observed resident cannot cancel another pod's finite
unconsumed reservation. A live original/private cgroup identity is checked on
observed allocations and both sides of each mode change.

The state format is version 4, with an explicit memory mode. Version 3 is accepted
only as finite and rewritten as typed state; unknown modes and malformed records
fail closed. A finite-to-observed resize records intent before removing the
runtime-owned private high/max, and only after the original is actually unlimited.
Every sandbox holder sharing that original receives its mode intent in one
atomic state commit before the first private-cap write. Restart replays those
intents so a replacement holder cannot leave a stale finite private cap or mix
finite future-credit accounting with observed charges.
Observed-to-finite requires the original's full enforced cap, charged bytes at or
below that cap and BE fencing before the finite contract becomes authoritative.
Startup, reconciliation and allocation fences discover an identity-matched
original mode change even when the CRI update was lost. They stage the same
typed shared-holder intents before restoring private caps. If kubelet reverses
a pending resize, a new durable intent re-proves its actual cap and charged
bytes; it never infers the outcome from an old private cap. No transition
restores or relaxes kubelet-owned caps.

Downgrade is explicit: an old version-3 reader rejects every nonempty version-4
journal (including unknown mode fields); do not restart it against live contracts.
Drain and remove all owning sandbox contracts through ordinary CRI lifecycle,
including Forget after metadata deletion. Only an exactly empty journal is
written in the legacy version-3 shape, preserving aggregate identity, kernel boot
and root ownership. Pending records, admitted/releasing contracts and released
tombstones are never downgraded. Read back that empty journal before the supported
immutable rollback. Never delete runtime state to force adoption. The old 9030 rollout is unchanged until the new
artifact, scoped effective config and typed readback are available.

## Node-pressure victim order

The matching native kubelet is part of the immutable stack, not an optional
stats feature gate. Stock memory eviction ranks above-request pods before
priority, which can select a protected Burstable service before a full-request
best-effort guest. The native policy first orders the admission-controlled
best-effort RuntimeClass ahead of protected pods, then retains stock ordering
within each tier. It changes neither measured memory statistics nor requests,
and does not disable node-pressure eviction. This closes host-growth pressure
which may reach an eviction threshold before any PSI stall occurs.

The supported kubelet ABI is the existing `v1.36.4`, with a source-qualified
native build identity. Foundation applies its digest through the ordinary
one-node `machine.kubelet.image` setting; no shared deployment is retargeted.
Its ordinary kubelet configuration stays unchanged, so the recorded effective
configuration hash remains an actual readback, not a guessed post-change hash.


## Immutable rollout and acceptance

Foundation packages native containerd in a custom Talos imager, the corrected
Kata shim in a system extension, and the same-ABI native kubelet in an image.
An extension cannot replace Talos's `/usr/bin/containerd`; changing only
installer-base does not change core boot assets. The stack has distinct immutable
component identities and an installer digest. Source publication never retargets
fleet pins, handlers or nodes.

The next native runtime supports requests-only protected residents through
observed protection, without a CNI, CSI or identity allowlist or a new hard cap.
Foundation must pair its typed preflight with the matching published native
artifact before selecting this mode. The older 9030 artifact and its finite-only
preflight remain a valid pair; source changes do not retarget either. The runtime never wipes
containerd state, tenant data or a volume to manufacture adoption success.

Use the existing Foundation node-day2 lifecycle on one drained supported worker,
with a staffed Ops window and a new immutable template record. Runtime and
process-ancestry readback precede eligibility. Retain full requests and the
closed safety gate until an independent verifier observes whole-host accounting,
actual balloon/free-page return and delay, simultaneous anonymous peaks, stalled
reclaim, best-effort-only failure and protected continuity. At least twenty
representative builds and the per-node-fit gate precede any measured-request
rollout. Source tests and healthy GitOps are not production pressure proof.

## Verification

Run deterministic protection-boundary tests and behavioral smoke exclusively
through `sylphx build run` in the linked source worktree. Native source patches
must compile against their pinned upstream releases. Small test-owned state
exercises admission and release ordering; it is not a substitute for independently
observed kernel and production pressure behavior. Never generate unsafe load on
a shared production node to manufacture acceptance.

The standalone standard-library module is `guard/`; `containerd/`, `kubelet/`
and the Go files under `patches/` are complete upstream integration sources,
compiled in their owning upstream packages by the build-time installers.
From this repository root, run the complete check through the linked worktree:

```sh
timeout 900 sylphx build run "$PWD" --queue-timeout 300s --timeout 600s -- \
  bash host-memory/check.sh
```

The check includes actual corrected binary version smoke and a failing-before /
passing-after kubelet memory-victim test. It generates no shared-node pressure.
