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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	. "sigs.k8s.io/controller-runtime/pkg/envtest/komega"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/poolselect"
)

var _ = Describe("IPAddressClaim pool selection", func() {
	var namespace string

	BeforeEach(func() {
		namespace = createNamespace()
	})

	When("a claim selects a pool by label", func() {
		const (
			namespacedPoolName = "local-prod"
			globalPoolName     = "global-prod"
		)

		BeforeEach(func() {
			nsPool := v1alpha2.InClusterIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      namespacedPoolName,
					Namespace: namespace,
					Labels:    map[string]string{"env": "prod"},
				},
				Spec: v1alpha2.InClusterIPPoolSpec{
					Addresses: []string{"10.20.0.1-10.20.0.254"},
					Prefix:    24,
					Gateway:   "10.20.0.1",
				},
			}
			Expect(k8sClient.Create(context.Background(), &nsPool)).To(Succeed())
			Eventually(Get(&nsPool)).Should(Succeed())

			// A global pool with the same labels must lose to the namespaced one.
			globalPool := v1alpha2.GlobalInClusterIPPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:   globalPoolName,
					Labels: map[string]string{"env": "prod"},
				},
				Spec: v1alpha2.InClusterIPPoolSpec{
					Addresses: []string{"10.30.0.1-10.30.0.254"},
					Prefix:    24,
					Gateway:   "10.30.0.1",
				},
			}
			Expect(k8sClient.Create(context.Background(), &globalPool)).To(Succeed())
			Eventually(Get(&globalPool)).Should(Succeed())
		})

		AfterEach(func() {
			deleteClaimUnchecked("label-selected-claim", namespace)
			deleteNamespacedPool(namespacedPoolName, namespace)
			deleteClusterScopedPool(globalPoolName)
		})

		It("allocates from the namespaced pool and records it by name", func() {
			claim := newClaim("label-selected-claim", namespace, inClusterIPPoolKind, poolselect.SelectorName)
			claim.Annotations = map[string]string{
				poolselect.SelectorAnnotation: `{"matchLabels":{"env":"prod"}}`,
			}
			Expect(k8sClient.Create(context.Background(), &claim)).To(Succeed())

			Eventually(func() ipamv1.IPAddressList {
				list := ipamv1.IPAddressList{}
				Expect(k8sClient.List(context.Background(), &list, client.InNamespace(namespace))).To(Succeed())
				return list
			}).Should(HaveField("Items", HaveLen(1)))

			address := ipamv1.IPAddress{}
			Eventually(func() error {
				return k8sClient.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "label-selected-claim"}, &address)
			}).Should(Succeed())

			// The recorded pool is the concrete namespaced pool, not the selector
			// marker, so a later reconcile can pin the allocation to it.
			Expect(address.Spec.PoolRef.Name).To(Equal(namespacedPoolName))
			Expect(address.Spec.PoolRef.Kind).To(Equal(inClusterIPPoolKind))
			Expect(address.Spec.Address).To(HavePrefix("10.20.0."))
		})
	})

	When("an explicit reference names a pool that does not exist", func() {
		AfterEach(func() {
			deleteClaimUnchecked("missing-pool-claim", namespace)
		})

		It("reports the claim instead of allocating from another pool", func() {
			claim := newClaim("missing-pool-claim", namespace, inClusterIPPoolKind, "does-not-exist")
			Expect(k8sClient.Create(context.Background(), &claim)).To(Succeed())

			Eventually(Object(&claim)).Should(
				HaveField("Status.Conditions", ContainElement(SatisfyAll(
					HaveField("Type", ipamv1.IPAddressClaimReadyCondition),
					HaveField("Status", metav1.ConditionFalse),
					HaveField("Reason", ipamv1.IPAddressClaimReadyPoolNotReadyReason),
				))))

			addresses := ipamv1.IPAddressList{}
			Expect(k8sClient.List(context.Background(), &addresses, client.InNamespace(namespace))).To(Succeed())
			Expect(addresses.Items).To(BeEmpty())
		})
	})
})
