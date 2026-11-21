# Cluster API IPAM Provider In Cluster

This is an [IPAM provider](https://github.com/kubernetes-sigs/cluster-api/blob/main/docs/proposals/20220125-ipam-integration.md#ipam-provider) for [Cluster API](https://github.com/kubernetes-sigs/cluster-api) that manages pools of IP addresses using Kubernetes resources. It serves as a reference implementation for IPAM providers, but can also be used as a simple replacement for DHCP.

IPAM providers allow to control how IP addresses are assigned to Cluster API Machines. It is usually only useful for non-cloud deployments. The infrastructure provider in use must support IPAM providers in order to use this provider.

## Features

- Manages IP Addresses in-cluster using custom Kubernetes resources
- Address and prefix pools can be cluster-wide or namespaced
- Pools can consist of subnets, arbitrary address ranges and/or individual addresses
- Both IPv4 and IPv6 are supported
- Individual addresses, ranges and subnets can be excluded from a pool
- Well-known reserved addresses are excluded by default, which can be configured per pool

## Setup via clusterctl

This provider comes with clusterctl support and can be installed using `clusterctl init --ipam in-cluster`.

## Usage

This provider comes with two resource pairs:

- Address allocation: `InClusterIPPool` (namespaced) and `GlobalInClusterIPPool` (cluster-wide)
- Prefix allocation (IPv6 only): `InClusterPrefixPool` (namespaced) and `GlobalInClusterPrefixPool` (cluster-wide)

Within each pair, the resources are functionally identical aside from scope. The address-pool examples below use `InClusterIPPool`, but also apply to `GlobalInClusterIPPool`.

### How a claim chooses its pool

A claim refers to a pool in one of two ways. The decision rules live in one
place, `internal/poolselect`, and both the implementation and callers follow the
same rules.

**Explicit reference.** `spec.poolRef.kind` and `spec.poolRef.name` name exactly
one pool. The `kind` distinguishes the category (address vs. prefix) and the
scope (namespaced vs. cluster-wide), so a namespaced and a cluster-wide pool may
share a name without being confused. A namespaced pool is only looked up in the
claim's own namespace. An explicit reference never falls back to another pool:
if the named pool does not exist, the claim reports `PoolNotFound` and no
address is allocated. A pool that is being deleted is reported as
`PoolTerminating` rather than skipped.

```yaml
spec:
  poolRef:
    apiGroup: ipam.cluster.x-k8s.io
    kind: GlobalInClusterIPPool
    name: inclusterippool-sample
```

**Label selector.** Set `spec.poolRef.name` to the reserved value
`pool-selector` and put a label selector on the
`ipam.cluster.x-k8s.io/pool-selector` annotation. The `kind` still fixes the
category; the scope is chosen by the rules below.

```yaml
metadata:
  annotations:
    ipam.cluster.x-k8s.io/pool-selector: '{"matchLabels":{"env":"prod"}}'
spec:
  poolRef:
    apiGroup: ipam.cluster.x-k8s.io
    kind: InClusterIPPool
    name: pool-selector
```

Selector rules, in order:

1. Only pools of the reference's category (address or prefix) that match the
   selector are considered.
2. A namespaced pool in the claim's namespace takes precedence over any
   cluster-wide pool.
3. If more than one pool matches at the preferred level, the reference is
   ambiguous (`Ambiguous`) and no pool is chosen. A pool is never picked
   arbitrarily.
4. If nothing matches, the claim reports `PoolNotFound`. There is no implicit
   default pool.

Within one category, an explicit `name` always wins over a selector carried by
the same reference.

**Capacity is not part of selection.** An exhausted pool still wins the
selection and the claim reports `PoolExhausted`; it does not spill over into a
pool with free space. This keeps addresses inside the network range the
reference asked for.

**Pools being deleted** take part in matching so a label-selected claim cannot
silently move to a different range; once such a pool wins the selection the
claim reports `PoolTerminating`.

### Existing allocations and label changes

An already allocated `IPAddress` records the pool it came from in
`spec.poolRef`. That recorded reference is authoritative: when a claim is
reconciled again, the allocation is pinned to the pool recorded on the address,
even if labels, selectors or the rules have changed. Pool labels changed at
runtime therefore affect new claims only; existing allocations are never
migrated. If the recorded pool has since been deleted, the allocation is kept
and the claim reports `ExistingPoolMissing` instead of selecting a new pool.
Claims allocated before this behaviour existed already carry the pool in
`spec.poolRef`, so they are recognised without migration.

### Changing a pool's addresses or prefix

Allocations are not migrated when a pool's `spec.addresses` or `spec.prefix`
changes. An address that is still inside the new range is kept as-is. An address
that falls outside is still kept, but it is counted in the pool's
`status.ipAddresses.outOfRange` so operators can handle it. This matches the
admission webhook, which rejects an update that would remove an in-use address
from the pool.

A simple pool that covers an entire /24 IPv4 network could look like this:

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterIPPool
metadata:
  name: inclusterippool-sample
spec:
  addresses:
    - 10.0.0.0/24
  prefix: 24
  gateway: 10.0.0.1
```

IPv6 is also supported, but a single pool can only consist of v4 **or** v6 addresses, not both. For simplicity we'll stick to IPv4 in the examples.

The `addresses` field supports CIDR notation, as well as arbitrary ranges and individual addresses. Using the `excludedAddresses` field, addresses, ranges or subnets can be excluded from the pool.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterIPPool
metadata:
  name: inclusterippool-sample
spec:
  addresses:
    - 10.0.0.0/24
    - 10.0.1.10-10.0.1.100
    - 10.0.2.1
    - 10.0.2.2
  excludeAddresses:
    - 10.10.0.16/28
    - 10.10.0.242
    - 10.0.1.25-10.0.1.30
  prefix: 22
  gateway: 10.0.0.1
```

Be aware that the prefix needs to cover all addresses that are part of the pool. The first network in the `addresses` list and the `prefix` field, which specifies the length of the prefix, is used to determine the prefix. In this case, `10.1.0.0/24` in the `addresses` list would lead to a validation error.

The `gateway` will never be allocated. By default, addresses that are usually reserved will not be allocated either. For v4 networks this is the first (network) and last (broadcast) address within the prefix. In the example above that would be `10.0.0.0` and `10.0.3.255` (the latter not being in the network anyway). For v6 networks the first address is excluded.

If you want to use all networks that are part of the prefix, you can set `allocateReservedIPAddresses` to `true`. In the example below, both `10.0.0.0` and `10.0.0.255` will be allocated. The gateway will still be excluded.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterIPPool
metadata:
  name: inclusterippool-sample
spec:
  addresses:
    - 10.0.0.0/24
  prefix: 24
  gateway: 10.0.0.1
  allocateReservedIPAddresses: true
```

### Prefix Pools

For prefix-per-consumer allocation (for example `/64` per claim), use `InClusterPrefixPool` or `GlobalInClusterPrefixPool`.

> [!NOTE]
> Currently only IPv6 prefixes supported.

```yaml
apiVersion: ipam.cluster.x-k8s.io/v1alpha2
kind: InClusterPrefixPool
metadata:
  name: inclusterprefixpool-sample
spec:
  prefixes:
    - fd00::/48
    - fd01::/48
  allocationPrefixLength: 64
  excludedPrefixes:
    - fd00:0:0:1::/64
  gateway: fd00::1
```

Key behavior:

- Each allocation is deterministic, first-fit, and non-overlapping
- `allocationPrefixLength` is immutable after creation, defaults to `64` when omitted
- `excludedPrefixes` are never allocatable, i.e. new claims skip them and updates are rejected if they would exclude already allocated prefixes
- `gateway` is informational in this case, it's copied to `IPAddress.spec.gateway` and does not affect reservation
- Allocated prefix data is stored in `IPAddress.spec.address` (base address) and `IPAddress.spec.prefix` (prefix length)

### IPv6 Status Limitations

This provider does fully support IPv6 pools, but they come with a small limitation related to the pools' status. Since the IPv6 address space is very large it exceeds the int64 range, making it cumbersome to calculate the total amount of available addresses for large pools. The status will therefore be limited to the maximum int64 value (2^64), even when more addresses are available. Since the Kubernetes api server will probably not allow storing enough resources to exhaust the pool anyway, we've decided to keep this limitation in favour of simpler implementation.

## Community, discussion, contribution, and support

The in-cluster IPAM provider is part of the cluster-api project. Please refer to it's [readme](https://github.com/kubernetes-sigs/cluster-api?tab=readme-ov-file#-community-discussion-contribution-and-support) for information on how to connect with the project.

The best way to reach the maintainers of this sub-project is the [#cluster-api](https://kubernetes.slack.com/archives/C8TSNPY4T) channel on the [Kubernetes Slack](https://slack.k8s.io).

Pull Requests and feedback on issues are very welcome! See the [issue tracker](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster/issues) if you're unsure where to start, especially the [Good first issue](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster/issues?q=is%3Aopen+is%3Aissue+label%3A%22good+first+issue%22) and [Help wanted](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster/issues?utf8=%E2%9C%93&q=is%3Aopen+is%3Aissue+label%3A%22help+wanted%22+) tags, and also feel free to reach out to discuss.

## Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](https://github.com/kubernetes-sigs/cluster-api-ipam-provider-in-cluster/blob/main/code-of-conduct.md).
