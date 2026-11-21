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

package webhooks

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/index"
)

// TestPoolShrinkConsistency pins the rule that out-of-range allocations are
// surfaced rather than migrated, and that the webhook refuses a shrink which
// would strand an in-use address. The same rule is exercised by the pool status
// tests, so a change to one path is caught by the other.
func TestPoolShrinkConsistency(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name          string
		newAddresses  []string
		newPrefix     int
		expectBlocked bool
	}{
		{
			name:          "keeping the address inside the new range is allowed",
			newAddresses:  []string{"10.0.0.0/16"},
			newPrefix:     16,
			expectBlocked: false,
		},
		{
			name:          "removing the in-use address from the pool is rejected",
			newAddresses:  []string{"10.0.5.0/24"},
			newPrefix:     24,
			expectBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			scheme := runtime.NewScheme()
			g.Expect(ipamv1.AddToScheme(scheme)).To(Succeed())

			namespacedPool := &v1alpha2.InClusterIPPool{
				ObjectMeta: metav1.ObjectMeta{Name: "my-pool", Namespace: "team-a"},
				Spec: v1alpha2.InClusterIPPoolSpec{
					Addresses: []string{"10.0.0.0/24"},
					Prefix:    24,
				},
			}
			namespacedPool.SetGroupVersionKind(v1alpha2.GroupVersion.WithKind("InClusterIPPool"))

			allocated := createIP("claim", "10.0.0.10", namespacedPool)
			allocated.Namespace = "team-a"

			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(allocated).
				WithIndex(&ipamv1.IPAddress{}, index.IPAddressPoolRefCombinedField, index.IPAddressByCombinedPoolRef).
				Build()

			webhook := InClusterIPPool{Client: fakeClient}

			oldPool := namespacedPool.DeepCopy()
			newPool := namespacedPool.DeepCopy()
			newPool.Spec.Addresses = tt.newAddresses
			if tt.newPrefix != 0 {
				newPool.Spec.Prefix = tt.newPrefix
			}

			_, err := webhook.ValidateUpdate(ctx, oldPool, newPool)
			if tt.expectBlocked {
				g.Expect(err).To(HaveOccurred(), "expected the shrink to be rejected")
			} else {
				g.Expect(err).NotTo(HaveOccurred(), "expected the update to be allowed")
			}
		})
	}
}
