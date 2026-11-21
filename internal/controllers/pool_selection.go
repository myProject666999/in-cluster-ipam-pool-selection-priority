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

package controllers

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/poolselect"
)

// selectedPool is the outcome of resolving a claim's pool reference.
type selectedPool struct {
	// Object is the selected pool with its GroupVersionKind populated, so that
	// owner references and IPAddress.Spec.PoolRef can be derived from it.
	Object client.Object
	// Reference is the effective reference. For a pinned existing allocation it
	// is the reference recorded on the IPAddress; otherwise it is the reference
	// resolved from the claim.
	Reference poolselect.Reference
}

// resolvePool decides which pool a claim allocates from.
//
// An already allocated IPAddress is authoritative: its Spec.PoolRef pins the
// allocation to the pool it came from, regardless of label or rule changes.
// Otherwise the claim's reference is parsed and resolved through
// [poolselect.Select].
func resolvePool(
	ctx context.Context,
	c client.Reader,
	claim *ipamv1.IPAddressClaim,
	family poolselect.Family,
) (*selectedPool, error) {
	// 1. An existing allocation pins its pool.
	existing := &ipamv1.IPAddress{}
	err := c.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Name}, existing)
	switch {
	case err == nil:
		if ref, ok := poolselect.ExistingReference(existing); ok && ref.Kind.Family == family {
			pool, err := fetchExplicitPool(ctx, c, claim.Namespace, ref)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return nil, &poolselect.Error{
						Kind: poolselect.ErrExistingPoolMissing,
						Message: fmt.Sprintf(
							"address is already allocated from %s %q, which no longer exists; the existing allocation is preserved and no other pool is selected",
							ref.Kind.Name, ref.Name),
						Cause: err,
					}
				}
				return nil, err
			}
			return &selectedPool{Object: pool, Reference: ref}, nil
		}
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("failed to fetch existing address: %w", err)
	}

	// 2. A new allocation follows the reference rules.
	ref, err := poolselect.ParseReference(claim.Spec.PoolRef, claim.Annotations)
	if err != nil {
		return nil, err
	}

	switch ref.Form {
	case poolselect.FormExplicit:
		pool, err := fetchExplicitPool(ctx, c, claim.Namespace, ref)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil, &poolselect.Error{
					Kind: poolselect.ErrPoolNotFound,
					Message: fmt.Sprintf(
						"%s %q does not exist; an explicit reference never falls back to another pool",
						ref.Kind.Name, ref.Name),
					Cause: err,
				}
			}
			return nil, err
		}
		if !pool.GetDeletionTimestamp().IsZero() {
			return nil, &poolselect.Error{
				Kind: poolselect.ErrPoolTerminating,
				Message: fmt.Sprintf(
					"%s %q is being deleted and cannot accept new allocations",
					ref.Kind.Name, ref.Name),
				Matches: []string{ref.Name},
			}
		}
		return &selectedPool{Object: pool, Reference: ref}, nil

	case poolselect.FormSelector:
		candidates, err := poolselect.Candidates(ctx, c, claim.Namespace, family)
		if err != nil {
			return nil, err
		}
		result, err := poolselect.Select(ref, claim.Namespace, candidates)
		if err != nil {
			return nil, err
		}
		// Objects returned by List do not carry TypeMeta. Stamp it via the
		// object's ObjectKind so the IPAddress owner reference and
		// IPAddress.Spec.PoolRef record the concrete pool, not an empty kind.
		result.Object.GetObjectKind().SetGroupVersionKind(v1alpha2.GroupVersion.WithKind(result.Kind.Name))
		return &selectedPool{
			Object: result.Object,
			Reference: poolselect.Reference{
				Form: poolselect.FormExplicit,
				Kind: result.Kind,
				Name: result.Object.GetName(),
			},
		}, nil
	}

	return nil, &poolselect.Error{
		Kind:    poolselect.ErrInvalidReference,
		Message: fmt.Sprintf("unknown reference form %q", ref.Form),
	}
}

// selectionErrorReason maps a classified selection error to the Ready condition
// reason and message reported on the claim. An empty reason means there is
// nothing to report.
func selectionErrorReason(err error) (string, string) {
	var selectionErr *poolselect.Error
	if !errors.As(err, &selectionErr) {
		return "", ""
	}
	switch selectionErr.Kind {
	case poolselect.ErrPoolExhausted:
		return ipamv1.IPAddressClaimReadyPoolExhaustedReason, selectionErr.Error()
	case poolselect.ErrPoolNotFound, poolselect.ErrExistingPoolMissing, poolselect.ErrPoolTerminating:
		return ipamv1.IPAddressClaimReadyPoolNotReadyReason, selectionErr.Error()
	default:
		return ipamv1.IPAddressClaimReadyAllocationFailedReason, selectionErr.Error()
	}
}

// fetchExplicitPool fetches exactly the pool named by ref. A namespaced kind is
// only looked up in claimNamespace and a cluster-wide kind only at cluster
// scope, so identically named pools of different scope stay distinct.
func fetchExplicitPool(
	ctx context.Context,
	c client.Reader,
	claimNamespace string,
	ref poolselect.Reference,
) (client.Object, error) {
	switch ref.Kind.Name {
	case inClusterIPPoolKind:
		pool := &v1alpha2.InClusterIPPool{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: claimNamespace, Name: ref.Name}, pool); err != nil {
			return nil, err
		}
		pool.SetGroupVersionKind(v1alpha2.GroupVersion.WithKind(inClusterIPPoolKind))
		return pool, nil
	case globalInClusterIPPoolKind:
		pool := &v1alpha2.GlobalInClusterIPPool{}
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name}, pool); err != nil {
			return nil, err
		}
		pool.SetGroupVersionKind(v1alpha2.GroupVersion.WithKind(globalInClusterIPPoolKind))
		return pool, nil
	case inClusterPrefixPoolKind:
		pool := &v1alpha2.InClusterPrefixPool{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: claimNamespace, Name: ref.Name}, pool); err != nil {
			return nil, err
		}
		pool.SetGroupVersionKind(v1alpha2.GroupVersion.WithKind(inClusterPrefixPoolKind))
		return pool, nil
	case globalInClusterPrefixPoolKind:
		pool := &v1alpha2.GlobalInClusterPrefixPool{}
		if err := c.Get(ctx, types.NamespacedName{Name: ref.Name}, pool); err != nil {
			return nil, err
		}
		pool.SetGroupVersionKind(v1alpha2.GroupVersion.WithKind(globalInClusterPrefixPoolKind))
		return pool, nil
	default:
		return nil, &poolselect.Error{
			Kind:    poolselect.ErrInvalidReference,
			Message: fmt.Sprintf("unsupported pool kind %q", ref.Kind.Name),
		}
	}
}
