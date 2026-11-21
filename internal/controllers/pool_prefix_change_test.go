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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
)

// TestPoolPrefixChange_KeepsInRangeAndFlagsOutOfRange documents what happens to
// already allocated addresses when a pool's prefix or address set changes: an
// address that stays inside the new pool is left alone, and an address that
// falls outside is not migrated but is counted in the pool's OutOfRange status
// so operators can act on it.
func TestPoolPrefixChange_KeepsInRangeAndFlagsOutOfRange(t *testing.T) {
	tests := []struct {
		name             string
		initialAddresses []string
		changedAddresses []string
		allocated        string
		wantOutOfRange   int
	}{
		{
			name:             "an address that stays inside the new prefix is retained",
			initialAddresses: []string{"10.0.0.0/24"},
			changedAddresses: []string{"10.0.0.0/22"},
			allocated:        "10.0.0.10",
			wantOutOfRange:   0,
		},
		{
			name:             "an address outside the new prefix is flagged, not moved",
			initialAddresses: []string{"10.0.0.0/24"},
			changedAddresses: []string{"10.0.5.0/24"},
			allocated:        "10.0.0.10",
			wantOutOfRange:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			pool := &v1alpha2.InClusterIPPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: selectionNamespace},
				Spec: v1alpha2.InClusterIPPoolSpec{
					Addresses: tt.initialAddresses,
					Prefix:    24,
				},
			}
			address := &ipamv1.IPAddress{
				ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: selectionNamespace},
				Spec: ipamv1.IPAddressSpec{
					PoolRef: ipamv1.IPPoolReference{
						APIGroup: v1alpha2.GroupVersion.Group,
						Kind:     inClusterIPPoolKind,
						Name:     "pool",
					},
					Address: tt.allocated,
					Prefix:  ptr.To(int32(24)),
				},
			}

			c := selectionClient(t, pool, address)

			pool.Spec.Addresses = tt.changedAddresses
			if err := c.Update(ctx, pool); err != nil {
				t.Fatalf("failed to update pool: %v", err)
			}

			// The pool controller recomputes status from the current spec and the
			// addresses that are in use.
			stored := &v1alpha2.InClusterIPPool{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pool), stored); err != nil {
				t.Fatalf("failed to fetch pool: %v", err)
			}
			stored.SetGroupVersionKind(v1alpha2.GroupVersion.WithKind(inClusterIPPoolKind))
			if _, err := genericReconcile(ctx, c, stored); err != nil {
				t.Fatalf("unexpected reconcile error: %v", err)
			}

			if stored.Status.Addresses == nil {
				t.Fatal("expected pool status to be set")
			}
			if got := stored.Status.Addresses.OutOfRange; got != tt.wantOutOfRange {
				t.Fatalf("expected OutOfRange %d, got %d", tt.wantOutOfRange, got)
			}

			got := &ipamv1.IPAddress{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(address), got); err != nil {
				t.Fatalf("failed to fetch address: %v", err)
			}
			if got.Spec.Address != tt.allocated {
				t.Fatalf("an allocation must not migrate or be rewritten; got %q", got.Spec.Address)
			}
		})
	}
}
