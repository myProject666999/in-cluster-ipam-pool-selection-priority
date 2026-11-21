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
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/index"
	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/internal/poolselect"
)

const (
	selectionNamespace      = "team-a"
	selectionOtherNamespace = "team-b"
)

func selectionScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha2.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha2 to scheme: %v", err)
	}
	if err := ipamv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add ipamv1 to scheme: %v", err)
	}
	return scheme
}

func selectionClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(selectionScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(
			&v1alpha2.InClusterIPPool{},
			&v1alpha2.GlobalInClusterIPPool{},
			&v1alpha2.InClusterPrefixPool{},
			&v1alpha2.GlobalInClusterPrefixPool{},
		).
		WithIndex(&ipamv1.IPAddress{}, index.IPAddressPoolRefCombinedField, index.IPAddressByCombinedPoolRef).
		Build()
}

func explicitClaim(name, kind, poolName string) *ipamv1.IPAddressClaim {
	return &ipamv1.IPAddressClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: selectionNamespace},
		Spec: ipamv1.IPAddressClaimSpec{
			PoolRef: ipamv1.IPPoolReference{
				APIGroup: v1alpha2.GroupVersion.Group,
				Kind:     kind,
				Name:     poolName,
			},
		},
	}
}

func selectorClaim(name, kind string, selector metav1.LabelSelector) *ipamv1.IPAddressClaim {
	claim := explicitClaim(name, kind, poolselect.SelectorName)
	encoded, err := selectorJSON(selector)
	if err != nil {
		panic(err)
	}
	claim.Annotations = map[string]string{poolselect.SelectorAnnotation: encoded}
	return claim
}

func selectorJSON(selector metav1.LabelSelector) (string, error) {
	encoded, err := json.Marshal(selector)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func labellessAddress(name, kind, poolName string) *ipamv1.IPAddress {
	return &ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: selectionNamespace},
		Spec: ipamv1.IPAddressSpec{
			PoolRef: ipamv1.IPPoolReference{
				APIGroup: v1alpha2.GroupVersion.Group,
				Kind:     kind,
				Name:     poolName,
			},
			Address: "10.0.0.10",
			Prefix:  ptr.To(int32(24)),
		},
	}
}

func TestResolvePool_DeclarationOutcomes(t *testing.T) {
	prod := map[string]string{"env": "prod"}
	dev := map[string]string{"env": "dev"}

	namespaced := func(name string, labels map[string]string) *v1alpha2.InClusterIPPool {
		return &v1alpha2.InClusterIPPool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: selectionNamespace, Labels: labels},
			Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.0.0.10-10.0.0.20"}, Prefix: 24},
		}
	}
	global := func(name string, labels map[string]string) *v1alpha2.GlobalInClusterIPPool {
		return &v1alpha2.GlobalInClusterIPPool{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.1.0.10-10.1.0.20"}, Prefix: 24},
		}
	}
	sameName := func() []client.Object {
		return []client.Object{
			namespaced("same", nil),
			global("same", nil),
		}
	}

	tests := []struct {
		name       string
		objects    []client.Object
		claim      *ipamv1.IPAddressClaim
		wantKind   string
		wantPool   string
		wantErr    poolselect.ErrorKind
		wantErrMsg string
	}{
		{
			name:     "only a global pool exists",
			objects:  []client.Object{global("shared", nil)},
			claim:    explicitClaim("c", globalInClusterIPPoolKind, "shared"),
			wantKind: globalInClusterIPPoolKind,
			wantPool: "shared",
		},
		{
			name:     "only a namespaced pool exists",
			objects:  []client.Object{namespaced("local", nil)},
			claim:    explicitClaim("c", inClusterIPPoolKind, "local"),
			wantKind: inClusterIPPoolKind,
			wantPool: "local",
		},
		{
			name:     "the same name exists in both kinds and the kind disambiguates",
			objects:  sameName(),
			claim:    explicitClaim("c", globalInClusterIPPoolKind, "same"),
			wantKind: globalInClusterIPPoolKind,
			wantPool: "same",
		},
		{
			name:    "an explicit pool that does not exist fails without falling back",
			objects: sameName(),
			claim:   explicitClaim("c", inClusterIPPoolKind, "missing"),
			// A pool with the requested name exists at the other scope; it must
			// not be used as a fallback.
			wantErr: poolselect.ErrPoolNotFound,
		},
		{
			name:     "a selector prefers the namespaced pool",
			objects:  []client.Object{global("global-prod", prod), namespaced("local-prod", prod)},
			claim:    selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod}),
			wantKind: inClusterIPPoolKind,
			wantPool: "local-prod",
		},
		{
			name:     "a selector uses the global pool when only it matches",
			objects:  []client.Object{global("global-prod", prod), namespaced("local-dev", dev)},
			claim:    selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod}),
			wantKind: globalInClusterIPPoolKind,
			wantPool: "global-prod",
		},
		{
			name:    "two matches at the preferred level are ambiguous",
			objects: []client.Object{namespaced("a", prod), namespaced("b", prod), global("global-prod", prod)},
			claim:   selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod}),
			wantErr: poolselect.ErrAmbiguous,
		},
		{
			name:    "a namespaced pool in another namespace is invisible",
			objects: []client.Object{&v1alpha2.InClusterIPPool{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: selectionOtherNamespace, Labels: prod}, Spec: v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.9.0.10-10.9.0.20"}, Prefix: 24}}},
			claim:   selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod}),
			wantErr: poolselect.ErrPoolNotFound,
		},
		{
			name:    "a terminating pool is reported rather than skipped",
			objects: []client.Object{terminatingPool(namespaced("going-away", nil))},
			claim:   explicitClaim("c", inClusterIPPoolKind, "going-away"),
			wantErr: poolselect.ErrPoolTerminating,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := selectionClient(t, tt.objects...)
			selection, err := resolvePool(context.Background(), c, tt.claim, poolselect.FamilyAddress)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %s, got pool %q", tt.wantErr, selection.Object.GetName())
				}
				if !poolselect.IsKind(err, tt.wantErr) {
					t.Fatalf("expected error %s, got %v", tt.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// The selected pool must carry its kind, whether it came from an
			// explicit Get or a selector-driven List, so the IPAddress records a
			// concrete poolRef and the owner reference is built correctly.
			if got := selection.Object.GetObjectKind().GroupVersionKind().Kind; got != tt.wantKind {
				t.Fatalf("expected kind %q, got %q", tt.wantKind, got)
			}
			if got := selection.Object.GetName(); got != tt.wantPool {
				t.Fatalf("expected pool %q, got %q", tt.wantPool, got)
			}
		})
	}
}

func terminatingPool(pool *v1alpha2.InClusterIPPool) *v1alpha2.InClusterIPPool {
	now := metav1.NewTime(time.Now())
	pool.DeletionTimestamp = &now
	// A finalizer keeps the fake client from dropping the object body, so the
	// pool is still listed and can be classified as terminating.
	pool.Finalizers = []string{ProtectPoolFinalizer}
	return pool
}

// TestResolvePool_ExistingAllocationIsPinned covers the compatibility rule: a
// claim that already has an address keeps that pool even when labels or
// selection rules change, and its recorded pool is the resolved one rather than
// the selector marker.
func TestResolvePool_ExistingAllocationIsPinned(t *testing.T) {
	prod := map[string]string{"env": "prod"}

	t.Run("a selector claim keeps the pool recorded on its existing address", func(t *testing.T) {
		original := &v1alpha2.InClusterIPPool{
			ObjectMeta: metav1.ObjectMeta{Name: "original", Namespace: selectionNamespace, Labels: map[string]string{"env": "dev"}},
			Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.0.0.10-10.0.0.20"}, Prefix: 24},
		}
		better := &v1alpha2.InClusterIPPool{
			ObjectMeta: metav1.ObjectMeta{Name: "better-match", Namespace: selectionNamespace, Labels: prod},
			Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.2.0.10-10.2.0.20"}, Prefix: 24},
		}
		claim := selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod})
		address := labellessAddress("c", inClusterIPPoolKind, "original")

		c := selectionClient(t, original, better, address)
		selection, err := resolvePool(context.Background(), c, claim, poolselect.FamilyAddress)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if selection.Object.GetName() != "original" {
			t.Fatalf("an existing allocation must not migrate; got %q", selection.Object.GetName())
		}
		if selection.Reference.Name != "original" || selection.Reference.Form != poolselect.FormExplicit {
			t.Fatalf("expected the recorded pool to be the pinned reference, got %+v", selection.Reference)
		}
	})

	t.Run("a claim whose recorded pool was deleted keeps its allocation and reports the missing pool", func(t *testing.T) {
		// The pool recorded on the address is gone, but another pool now matches
		// the selector. The allocation must not silently move.
		better := &v1alpha2.InClusterIPPool{
			ObjectMeta: metav1.ObjectMeta{Name: "better-match", Namespace: selectionNamespace, Labels: prod},
			Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.2.0.10-10.2.0.20"}, Prefix: 24},
		}
		claim := selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod})
		address := labellessAddress("c", inClusterIPPoolKind, "deleted-pool")

		c := selectionClient(t, better, address)
		_, err := resolvePool(context.Background(), c, claim, poolselect.FamilyAddress)
		if !poolselect.IsKind(err, poolselect.ErrExistingPoolMissing) {
			t.Fatalf("expected ExistingPoolMissing, got %v", err)
		}
	})
}

// TestAddressHandler_ExhaustedPoolIsReported verifies that an exhausted pool is
// still the selected pool and the claim reports exhaustion rather than moving
// to another pool with free space.
func TestAddressHandler_ExhaustedPoolIsReported(t *testing.T) {
	empty := &v1alpha2.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "full", Namespace: selectionNamespace},
		Spec: v1alpha2.InClusterIPPoolSpec{
			Addresses:                   []string{"10.0.0.10"},
			Prefix:                      24,
			AllocateReservedIPAddresses: true,
		},
	}
	spare := &v1alpha2.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "spare", Namespace: selectionNamespace},
		Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.3.0.10-10.3.0.20"}, Prefix: 24},
	}
	// Another claim already consumed the single address of the "full" pool.
	occupied := labellessAddress("other-claim", inClusterIPPoolKind, "full")
	occupied.Spec.Address = "10.0.0.10"

	claim := explicitClaim("wants-address", inClusterIPPoolKind, "full")

	c := selectionClient(t, empty, spare, occupied)
	handler := &IPAddressClaimHandler{Client: c, claim: claim}

	pool, _, err := handler.FetchPool(context.Background())
	if err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}
	if pool == nil || pool.GetName() != "full" {
		t.Fatalf("expected the fully allocated pool to still be selected, got %v", pool)
	}

	address := ipamv1.IPAddress{
		ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace},
	}
	if _, err := handler.EnsureAddress(context.Background(), &address); err == nil {
		t.Fatal("expected the exhausted pool to report exhaustion")
	} else if !poolselect.IsKind(err, poolselect.ErrPoolExhausted) {
		t.Fatalf("expected PoolExhausted, got %v", err)
	}

	reason, message := handler.SelectionError()
	if reason != ipamv1.IPAddressClaimReadyPoolExhaustedReason {
		t.Fatalf("expected PoolExhausted reason, got %q", reason)
	}
	if message == "" {
		t.Fatal("expected an explanatory message")
	}
	// The address must have been left unallocated: no silent switch to "spare".
	if address.Spec.Address != "" {
		t.Fatalf("expected no address to be allocated, got %q", address.Spec.Address)
	}
}

// TestAddressHandler_RecordsResolvedPool verifies that a selector-based claim
// records the resolved pool, not the selector marker, on the IPAddress it
// creates.
func TestAddressHandler_RecordsResolvedPool(t *testing.T) {
	prod := map[string]string{"env": "prod"}
	pool := &v1alpha2.InClusterIPPool{
		ObjectMeta: metav1.ObjectMeta{Name: "local-prod", Namespace: selectionNamespace, Labels: prod},
		Spec:       v1alpha2.InClusterIPPoolSpec{Addresses: []string{"10.0.0.10-10.0.0.20"}, Prefix: 24},
	}
	claim := selectorClaim("c", inClusterIPPoolKind, metav1.LabelSelector{MatchLabels: prod})

	c := selectionClient(t, pool)
	handler := &IPAddressClaimHandler{Client: c, claim: claim}

	if _, _, err := handler.FetchPool(context.Background()); err != nil {
		t.Fatalf("unexpected fetch error: %v", err)
	}

	if handler.poolRef.Name != "local-prod" {
		t.Fatalf("expected the resolved pool name, got %q", handler.poolRef.Name)
	}
	if handler.poolRef.Kind != inClusterIPPoolKind {
		t.Fatalf("expected the resolved pool kind, got %q", handler.poolRef.Kind)
	}
}
