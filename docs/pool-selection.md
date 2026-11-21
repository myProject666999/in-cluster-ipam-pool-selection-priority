# Pool selection rules

This note is the delivery record for the pool-selection work. It states the
complete decision rules, what changed relative to the previous behaviour, and
the compatibility stance for claims that were allocated under the old rules.

## Decision rules

All rules are implemented once in `internal/poolselect` and consumed by both
claim handlers through `internal/controllers/pool_selection.go`. The rules are
pure functions over in-memory descriptors and are covered by unit tests that
need no API server.

### Reference forms

A claim expresses its pool reference as either **explicit** or **selector**.

| Form | How it is written | What it identifies |
| --- | --- | --- |
| Explicit | `spec.poolRef.kind` + a concrete `spec.poolRef.name` | Exactly one pool. |
| Selector | `spec.poolRef.kind`, `spec.poolRef.name: pool-selector`, and the `ipam.cluster.x-k8s.io/pool-selector` annotation holding a `metav1.LabelSelector` | A set of candidate pools of one category. |

The `kind` always fixes the **category** (address vs. prefix) and, for an
explicit reference, the **scope** (namespaced vs. cluster-wide). A namespaced
pool and a cluster-wide pool may therefore share a name, and the reference still
identifies exactly one of them.

Within a single category, an explicit `name` takes precedence over a selector
carried by the same reference. The selector marker `pool-selector` is reserved:
using it without the annotation is an invalid reference, so a forgotten
annotation cannot silently become a lookup of a pool literally named
`pool-selector`.

There is **no implicit default pool**. A reference that names neither a pool nor
a selector is invalid.

### Explicit reference

- Look only in the claim's namespace for a namespaced kind; look cluster-wide
  for a global kind.
- If the pool exists, use it.
- If it does not exist, fail with `NotFound`. Do not fall back to any other
  pool, even one with the same name at the other scope.
- If it is being deleted, fail with `PoolTerminating`.

### Selector reference

Applied in order:

1. Consider only pools of the reference's **category** whose labels match.
2. A **namespaced** pool in the claim's namespace takes precedence over any
   cluster-wide pool.
3. If **more than one** pool matches at the preferred level, fail with
   `Ambiguous`. Never pick one arbitrarily.
4. Otherwise use the single namespaced match if there is one, else the single
   cluster-wide match.
5. If nothing matches, fail with `NotFound`. No default pool is applied.
6. If the chosen pool is being deleted, fail with `PoolTerminating`. Terminating
   pools therefore take part in matching, so a label-selected claim cannot
   silently move to a different network range.

### Capacity

Capacity is **not** a selection input. A pool that is already exhausted still
wins the selection; the claim then reports `PoolExhausted`. It does not spill
over into a sibling pool that still has free space. This keeps the allocated
address inside the range the reference asked for. It is deliberately different
from a "prefer a pool with free space" rule.

### Existing allocations

An allocated `IPAddress` records its pool in `spec.poolRef`. On every reconcile
the recorded reference is authoritative:

- The allocation is pinned to the pool recorded on the address, even if labels,
  selectors or the rules change.
- If the recorded pool no longer exists, the allocation is kept and the claim
  reports `ExistingPoolMissing`. No new pool is selected.

The claim's own `spec.poolRef` is only consulted when there is no existing
allocation. Pool labels changed at runtime therefore affect new claims only.

### Pool address and prefix changes

- Allocations are never migrated.
- An allocated address still inside the new range is kept as-is.
- An allocated address outside the new range is kept, and counted in the pool's
  `status.ipAddresses.outOfRange`.
- The admission webhook rejects a pool update that would remove an in-use
  address from the pool. Status reporting and webhook validation therefore
  agree: an out-of-range occupancy is either prevented (webhook) or surfaced
  (status), never silently migrated.

## Error categories

`internal/poolselect` exposes stable categories via `poolselect.Error` and
`poolselect.IsKind`, and maps them to the claim's `Ready` condition reason:

| Category | Ready reason | Meaning |
| --- | --- | --- |
| `InvalidReference` | `AllocationFailed` | Reference names no pool, unknown kind, bad selector, or marker without annotation. |
| `NotFound` | `PoolNotReady` | Named pool missing, or a selector matched nothing. |
| `Ambiguous` | `AllocationFailed` | More than one pool matched at the preferred level. |
| `PoolTerminating` | `PoolNotReady` | The chosen pool is being deleted. |
| `PoolExhausted` | `PoolExhausted` | The chosen pool has no free address or prefix. |
| `ExistingPoolMissing` | `PoolNotReady` | The pool recorded on an existing allocation is gone. |

Selection failures are reported on the claim status and are not retried in a
tight loop: the reference, the labels, or the pools have to change before the
outcome can change. Reporting is done through the optional
`ipamutil.SelectionErrorReporter` interface, so third-party `ClaimHandler`
implementations keep compiling unchanged.

## Behavioural differences from the previous implementation

Before this change, `FetchPool` fetched exactly the kind and name in
`spec.poolRef` and had no selector support. The differences introduced here are:

1. **New: label-selector references.** `spec.poolRef.name: pool-selector` plus
   the `ipam.cluster.x-k8s.io/pool-selector` annotation. Namespaced-first
   precedence, ambiguity as an error, no default pool.
2. **New: status is reported.** A claim whose pool cannot be selected now gets a
   `Ready=False` condition with a stable reason. Previously a missing pool was
   logged and the claim was left untouched.
3. **New: explicit-terminating pools are reported.** Previously a terminating
   pool was used (or failed depending on timing); now it is reported as
   `PoolTerminating`.
4. **New: exhaustion is reported, not retried.** Previously allocation failure
   was retried; now exhaustion is a reported condition, and the exhausted pool
   is not skipped in favour of another.
5. **New: existing allocations are pinned.** A re-reconcile resolves the pool
   from the recorded `IPAddress.spec.poolRef` rather than re-reading the claim's
   reference. This is what guarantees that a label change or a rule change does
   not migrate an existing allocation.

Everything that previously worked continues to work: an explicit reference with
an existing pool behaves exactly as before, and `IPAddress.spec.poolRef` still
records the concrete pool name and kind.

## Compatibility

- **Existing claims keep their pool.** Claims allocated under the old rules
  already carry the concrete pool in `IPAddress.spec.poolRef`. That recorded
  reference is now what gets resolved on subsequent reconciles, so they are
  recognised without any migration step.
- **Only new claims use the new rules.** A claim with no existing allocation
  goes through `poolselect`; one with an allocation is pinned.
- **Deleted referenced pools.** If the pool an existing allocation points at is
  deleted, the allocation is not moved; the claim reports
  `ExistingPoolMissing`. Operators decide whether to release the claim.
- **Unchanged reference syntax.** No API field was added or changed. Explicit
  references keep working byte-for-byte. Selectors are opt-in via the reserved
  name and annotation.

## Tests

All selection use cases run on in-memory objects:

- `internal/poolselect/poolselect_test.go` — the rule matrix: only a global
  pool, only a namespaced pool, both with the same name, namespaced-first when
  both match, ambiguity when several match at one level, a missing explicit
  pool, an exhausted pool still selected, an existing allocation that does not
  migrate, and explicit-name-beats-selector.
- `internal/controllers/pool_selection_test.go` — the same matrix through
  `resolvePool` and the address handler on a fake client, including that the
  resolved pool (not the selector marker) is recorded, and that an exhausted
  pool reports exhaustion without touching `IPAddress.spec.address`.
- `internal/controllers/pool_prefix_change_test.go` — prefix/address changes
  keep in-range allocations, flag out-of-range ones in pool status, and never
  rewrite the address.
- `internal/webhooks/pool_shrink_consistency_test.go` — the webhook rejects a
  shrink that would strand an in-use address, consistent with the status rule.
