/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package poolselect implements the deterministic rules that decide which pool
// an IPAddressClaim allocates from.
//
// The rules are intentionally enforced in one place and are pure functions over
// in-memory pool descriptors, so that implementation and callers cannot drift
// apart and the rules can be exercised without an API server.
//
// # Reference forms
//
// A claim can refer to pools in one of two forms:
//
//   - Explicit: spec.poolRef.kind and spec.poolRef.name identify exactly one
//     pool. The kind disambiguates the pool family (address vs. prefix) and the
//     scope (namespaced vs. cluster-wide), so a namespaced and a cluster-wide
//     pool may share a name without being confused.
//
//   - Selector: spec.poolRef.kind identifies the pool family,
//     spec.poolRef.name is the reserved marker [SelectorName] and the
//     [SelectorAnnotation] annotation carries a metav1.LabelSelector. Scope is
//     not part of the reference: namespaced pools in the claim's namespace take
//     precedence over cluster-wide pools, and more than one match at the
//     preferred level is an error rather than an arbitrary pick.
//
// An explicit name always wins over a selector carried by the same reference.
// There is no implicit "default pool": a reference that identifies neither a
// name nor a selector is invalid.
package poolselect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
)

const (
	// SelectorAnnotation is set on an IPAddressClaim to select a pool by its
	// labels. The value is a JSON-encoded metav1.LabelSelector.
	SelectorAnnotation = "ipam.cluster.x-k8s.io/pool-selector"

	// SelectorName is the reserved spec.poolRef.name value that marks a
	// label-selector reference. The API requires poolRef.name, so selector
	// references carry this marker instead of a pool name. The value is
	// reserved: it is never resolved as a pool name, and using it without the
	// [SelectorAnnotation] annotation is an invalid reference.
	SelectorName = "pool-selector"
)

// Scope describes whether a pool kind is namespaced or cluster-wide.
type Scope string

const (
	// ScopeNamespaced marks a pool that only a claim in the same namespace may use.
	ScopeNamespaced Scope = "namespaced"

	// ScopeCluster marks a pool that any namespace may use.
	ScopeCluster Scope = "cluster"
)

// Family groups the address and prefix pool kinds.
type Family string

const (
	// FamilyAddress groups InClusterIPPool and GlobalInClusterIPPool.
	FamilyAddress Family = "address"

	// FamilyPrefix groups InClusterPrefixPool and GlobalInClusterPrefixPool.
	FamilyPrefix Family = "prefix"
)

// Kind describes a pool kind: its name, family and scope.
type Kind struct {
	Name   string
	Family Family
	Scope  Scope
}

// Pool kind names. They match the Kubernetes kinds and are the keys of
// [SupportedKinds].
const (
	KindInClusterIPPool           = "InClusterIPPool"
	KindGlobalInClusterIPPool     = "GlobalInClusterIPPool"
	KindInClusterPrefixPool       = "InClusterPrefixPool"
	KindGlobalInClusterPrefixPool = "GlobalInClusterPrefixPool"
)

// SupportedKinds lists the pool kinds this provider understands. The map is the
// single source of truth for category and scope disambiguation.
var SupportedKinds = map[string]Kind{
	KindInClusterIPPool: {
		Name:   KindInClusterIPPool,
		Family: FamilyAddress,
		Scope:  ScopeNamespaced,
	},
	KindGlobalInClusterIPPool: {
		Name:   KindGlobalInClusterIPPool,
		Family: FamilyAddress,
		Scope:  ScopeCluster,
	},
	KindInClusterPrefixPool: {
		Name:   KindInClusterPrefixPool,
		Family: FamilyPrefix,
		Scope:  ScopeNamespaced,
	},
	KindGlobalInClusterPrefixPool: {
		Name:   KindGlobalInClusterPrefixPool,
		Family: FamilyPrefix,
		Scope:  ScopeCluster,
	},
}

// KindsForFamily returns the kinds of a family, namespaced kind first.
func KindsForFamily(family Family) []Kind {
	kinds := make([]Kind, 0, 2)
	for _, kind := range SupportedKinds {
		if kind.Family == family {
			kinds = append(kinds, kind)
		}
	}
	sort.Slice(kinds, func(i, j int) bool {
		return kinds[i].Scope == ScopeNamespaced && kinds[j].Scope != ScopeNamespaced
	})
	return kinds
}

// Form distinguishes the two reference forms.
type Form string

const (
	// FormExplicit is a reference that names exactly one pool.
	FormExplicit Form = "explicit"

	// FormSelector is a reference that matches pools by label.
	FormSelector Form = "selector"
)

// Reference is a parsed and validated pool reference.
type Reference struct {
	Form     Form
	Kind     Kind
	Name     string
	Selector labels.Selector
	// RawSelector is kept for diagnostics.
	RawSelector *metav1.LabelSelector
}

// Candidate is an in-memory view of a pool that selection can choose from.
type Candidate struct {
	Object      client.Object
	Kind        Kind
	Namespace   string
	Labels      map[string]string
	Terminating bool
}

// Result is a selected pool.
type Result struct {
	Object client.Object
	Kind   Kind
	Scope  Scope
}

// ErrorKind classifies selection failures so callers can branch on a stable
// category instead of on message text.
type ErrorKind string

const (
	// ErrInvalidReference means the reference does not identify a pool:
	// an unknown or empty kind, a missing name, a selector that cannot be
	// parsed, or a selector marker without its annotation.
	ErrInvalidReference ErrorKind = "InvalidReference"

	// ErrPoolNotFound means an explicit reference named a pool that does not
	// exist, or an explicit existing allocation points at a pool that has since
	// been deleted. It is never satisfied by falling back to another pool.
	ErrPoolNotFound ErrorKind = "NotFound"

	// ErrAmbiguous means a selector matched more than one pool at the
	// preferred scope. No pool is chosen.
	ErrAmbiguous ErrorKind = "Ambiguous"

	// ErrPoolTerminating means the selected pool is being deleted and therefore
	// does not accept new allocations.
	ErrPoolTerminating ErrorKind = "PoolTerminating"

	// ErrPoolExhausted means the selected pool has no free address or prefix.
	// The pool is still selected; the claim reports exhaustion rather than
	// switching to a different pool.
	ErrPoolExhausted ErrorKind = "PoolExhausted"

	// ErrExistingPoolMissing means a claim already has an allocated address, but
	// the pool recorded on that address no longer exists. The allocation is kept
	// as-is; no new pool is selected.
	ErrExistingPoolMissing ErrorKind = "ExistingPoolMissing"
)

// Error is a classified selection error.
type Error struct {
	Kind    ErrorKind
	Message string
	// Matches holds the names of the pools that produced the error, when
	// applicable (ambiguity, terminating).
	Matches []string
	// Cause is the underlying error, when there is one. It may be an
	// apierrors NotFound so callers can still treat a missing explicit pool
	// specially.
	Cause error
}

func (e *Error) Error() string {
	if len(e.Matches) == 0 {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Kind, e.Message, strings.Join(e.Matches, ", "))
}

// Unwrap exposes the underlying cause, if any.
func (e *Error) Unwrap() error {
	return e.Cause
}

// IsKind reports whether err is, or wraps, a selection error of the given
// category.
func IsKind(err error, kind ErrorKind) bool {
	var selErr *Error
	return errors.As(err, &selErr) && selErr.Kind == kind
}

// ParseReference validates a claim's pool reference and classifies its form.
func ParseReference(poolRef ipamv1.IPPoolReference, annotations map[string]string) (Reference, error) {
	kind, ok := SupportedKinds[poolRef.Kind]
	if !ok {
		if poolRef.Kind == "" {
			return Reference{}, &Error{
				Kind:    ErrInvalidReference,
				Message: "spec.poolRef.kind is required and must identify an in-cluster pool kind",
			}
		}
		return Reference{}, &Error{
			Kind:    ErrInvalidReference,
			Message: fmt.Sprintf("spec.poolRef.kind %q is not a supported in-cluster pool kind", poolRef.Kind),
		}
	}

	rawSelector, hasSelector := annotations[SelectorAnnotation]
	if poolRef.Name == SelectorName {
		// The marker is reserved: it must always be paired with the annotation,
		// otherwise a forgotten annotation would silently turn into a lookup of
		// a pool literally named "pool-selector".
		if !hasSelector {
			return Reference{}, &Error{
				Kind: ErrInvalidReference,
				Message: fmt.Sprintf(
					"spec.poolRef.name %q marks a label selector reference, but annotation %q is missing",
					SelectorName, SelectorAnnotation),
			}
		}
		selector := &metav1.LabelSelector{}
		if err := json.Unmarshal([]byte(rawSelector), selector); err != nil {
			return Reference{}, &Error{
				Kind:    ErrInvalidReference,
				Message: fmt.Sprintf("annotation %q is not a valid label selector: %v", SelectorAnnotation, err),
			}
		}
		compiled, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return Reference{}, &Error{
				Kind:    ErrInvalidReference,
				Message: fmt.Sprintf("annotation %q is not a valid label selector: %v", SelectorAnnotation, err),
			}
		}
		return Reference{
			Form:        FormSelector,
			Kind:        kind,
			Selector:    compiled,
			RawSelector: selector,
		}, nil
	}

	// An explicit name always wins, including over a selector carried by the
	// same reference. This keeps a stated name authoritative.
	if poolRef.Name == "" {
		return Reference{}, &Error{
			Kind:    ErrInvalidReference,
			Message: "spec.poolRef.name is required unless a label selector reference is used",
		}
	}

	return Reference{
		Form: FormExplicit,
		Kind: kind,
		Name: poolRef.Name,
	}, nil
}

// Select applies the decision rules to a reference and the candidate pools
// visible to the claim.
//
// claimNamespace is the namespace of the claim; it bounds namespaced
// candidates. candidates may contain pools of both scopes and of both kinds in
// a family; the reference's family is used to ignore unrelated kinds.
func Select(ref Reference, claimNamespace string, candidates []Candidate) (Result, error) {
	switch ref.Form {
	case FormExplicit:
		return selectExplicit(ref, claimNamespace, candidates)
	case FormSelector:
		return selectByLabels(ref, claimNamespace, candidates)
	default:
		return Result{}, &Error{
			Kind:    ErrInvalidReference,
			Message: fmt.Sprintf("unknown reference form %q", ref.Form),
		}
	}
}

func selectExplicit(ref Reference, claimNamespace string, candidates []Candidate) (Result, error) {
	for _, candidate := range candidates {
		if candidate.Kind.Name != ref.Kind.Name {
			continue
		}
		if candidate.Object.GetName() != ref.Name {
			continue
		}
		if candidate.Kind.Scope == ScopeNamespaced && candidate.Namespace != claimNamespace {
			continue
		}
		if candidate.Terminating {
			return Result{}, &Error{
				Kind:    ErrPoolTerminating,
				Message: fmt.Sprintf("%s %q is being deleted and cannot accept new allocations", ref.Kind.Name, ref.Name),
				Matches: []string{candidateLabel(candidate)},
			}
		}
		return Result{
			Object: candidate.Object,
			Kind:   candidate.Kind,
			Scope:  candidate.Kind.Scope,
		}, nil
	}

	return Result{}, &Error{
		Kind: ErrPoolNotFound,
		Message: fmt.Sprintf(
			"%s %q was not found; an explicit reference never falls back to another pool",
			ref.Kind.Name, ref.Name,
		),
	}
}

func selectByLabels(ref Reference, claimNamespace string, candidates []Candidate) (Result, error) {
	var namespacedMatches, clusterMatches []Candidate

	for _, candidate := range candidates {
		if candidate.Kind.Family != ref.Kind.Family {
			continue
		}
		if !ref.Selector.Matches(labels.Set(candidate.Labels)) {
			continue
		}
		switch candidate.Kind.Scope {
		case ScopeNamespaced:
			// A namespaced pool is only visible to claims in its own namespace.
			if candidate.Namespace == claimNamespace {
				namespacedMatches = append(namespacedMatches, candidate)
			}
		case ScopeCluster:
			clusterMatches = append(clusterMatches, candidate)
		}
	}

	var (
		winner Candidate
		found  bool
	)
	switch {
	case len(namespacedMatches) > 1:
		return Result{}, ambiguousError(ref, ScopeNamespaced, namespacedMatches)
	case len(namespacedMatches) == 1:
		winner, found = namespacedMatches[0], true
	case len(clusterMatches) > 1:
		return Result{}, ambiguousError(ref, ScopeCluster, clusterMatches)
	case len(clusterMatches) == 1:
		winner, found = clusterMatches[0], true
	}

	if !found {
		return Result{}, &Error{
			Kind:    ErrPoolNotFound,
			Message: "no pool matched the label selector at the namespaced or cluster scope; no default pool is applied",
		}
	}

	// Terminating pools take part in matching so that a label-selected claim
	// does not silently move to a pool in a different network range. Once such
	// a pool wins the selection, the claim reports it instead of allocating.
	if winner.Terminating {
		return Result{}, &Error{
			Kind:    ErrPoolTerminating,
			Message: fmt.Sprintf("%s is being deleted and cannot accept new allocations", candidateLabel(winner)),
			Matches: []string{candidateLabel(winner)},
		}
	}

	return selected(winner), nil
}

func selected(candidate Candidate) Result {
	return Result{
		Object: candidate.Object,
		Kind:   candidate.Kind,
		Scope:  candidate.Kind.Scope,
	}
}

func ambiguousError(_ Reference, scope Scope, matches []Candidate) *Error {
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, candidateLabel(match))
	}
	slices.Sort(names)
	return &Error{
		Kind: ErrAmbiguous,
		Message: fmt.Sprintf(
			"label selector matched %d %s pools; refusing to pick one arbitrarily",
			len(matches), scope,
		),
		Matches: names,
	}
}

func candidateLabel(candidate Candidate) string {
	if candidate.Kind.Scope == ScopeNamespaced {
		return fmt.Sprintf("%s/%s/%s", candidate.Namespace, candidate.Kind.Name, candidate.Object.GetName())
	}
	return fmt.Sprintf("%s/%s", candidate.Kind.Name, candidate.Object.GetName())
}

// ExistingReference returns the reference recorded on an already allocated
// IPAddress. Existing allocations are pinned to the pool they were created
// from; they are never re-selected when labels, selectors or selection rules
// change.
func ExistingReference(address *ipamv1.IPAddress) (Reference, bool) {
	if address == nil || address.Spec.PoolRef.Name == "" {
		return Reference{}, false
	}
	kind, ok := SupportedKinds[address.Spec.PoolRef.Kind]
	if !ok {
		return Reference{}, false
	}
	return Reference{
		Form: FormExplicit,
		Kind: kind,
		Name: address.Spec.PoolRef.Name,
	}, true
}

// Candidates lists every pool that a claim in claimNamespace could be selected
// against: the namespaced kinds in that namespace and both cluster-wide kinds.
// Listing is done per kind rather than per family so only supported kinds are
// ever considered.
func Candidates(ctx context.Context, c client.Reader, claimNamespace string, family Family) ([]Candidate, error) {
	candidates := []Candidate{}
	for _, kind := range KindsForFamily(family) {
		switch kind.Name {
		case "InClusterIPPool":
			list := &v1alpha2.InClusterIPPoolList{}
			if err := c.List(ctx, list, client.InNamespace(claimNamespace)); err != nil {
				return nil, fmt.Errorf("failed to list %s: %w", kind.Name, err)
			}
			for i := range list.Items {
				candidates = append(candidates, newCandidate(kind, &list.Items[i], list.Items[i].Namespace))
			}
		case "GlobalInClusterIPPool":
			list := &v1alpha2.GlobalInClusterIPPoolList{}
			if err := c.List(ctx, list); err != nil {
				return nil, fmt.Errorf("failed to list %s: %w", kind.Name, err)
			}
			for i := range list.Items {
				candidates = append(candidates, newCandidate(kind, &list.Items[i], list.Items[i].Namespace))
			}
		case "InClusterPrefixPool":
			list := &v1alpha2.InClusterPrefixPoolList{}
			if err := c.List(ctx, list, client.InNamespace(claimNamespace)); err != nil {
				return nil, fmt.Errorf("failed to list %s: %w", kind.Name, err)
			}
			for i := range list.Items {
				candidates = append(candidates, newCandidate(kind, &list.Items[i], list.Items[i].Namespace))
			}
		case "GlobalInClusterPrefixPool":
			list := &v1alpha2.GlobalInClusterPrefixPoolList{}
			if err := c.List(ctx, list); err != nil {
				return nil, fmt.Errorf("failed to list %s: %w", kind.Name, err)
			}
			for i := range list.Items {
				candidates = append(candidates, newCandidate(kind, &list.Items[i], list.Items[i].Namespace))
			}
		}
	}
	return candidates, nil
}

func newCandidate(kind Kind, object client.Object, namespace string) Candidate {
	return Candidate{
		Object:      object,
		Kind:        kind,
		Namespace:   namespace,
		Labels:      object.GetLabels(),
		Terminating: !object.GetDeletionTimestamp().IsZero(),
	}
}
