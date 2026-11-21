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

package poolselect

import (
	"encoding/json"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ipamv1 "sigs.k8s.io/cluster-api/api/ipam/v1beta2"

	"sigs.k8s.io/cluster-api-ipam-provider-in-cluster/api/v1alpha2"
)

func selectorJSON(selector metav1.LabelSelector) (string, error) {
	encoded, err := json.Marshal(selector)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

const claimNamespace = "team-a"

func namespacedIPPool(name, namespace string, labels map[string]string) Candidate {
	pool := &v1alpha2.InClusterIPPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
	return Candidate{Object: pool, Kind: SupportedKinds["InClusterIPPool"], Namespace: namespace, Labels: labels}
}

func globalIPPool(name string, labels map[string]string) Candidate {
	pool := &v1alpha2.GlobalInClusterIPPool{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	return Candidate{Object: pool, Kind: SupportedKinds["GlobalInClusterIPPool"], Labels: labels}
}

func namespacedPrefixPool(name, namespace string, labels map[string]string) Candidate {
	pool := &v1alpha2.InClusterPrefixPool{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}}
	return Candidate{Object: pool, Kind: SupportedKinds["InClusterPrefixPool"], Namespace: namespace, Labels: labels}
}

func globalPrefixPool(name string, labels map[string]string) Candidate {
	pool := &v1alpha2.GlobalInClusterPrefixPool{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	return Candidate{Object: pool, Kind: SupportedKinds["GlobalInClusterPrefixPool"], Labels: labels}
}

func terminating(candidate Candidate) Candidate {
	now := metav1.Now()
	candidate.Object.SetDeletionTimestamp(&now)
	candidate.Terminating = true
	return candidate
}

func mustReference(t *testing.T, poolRef ipamv1.IPPoolReference, annotations map[string]string) Reference {
	t.Helper()
	ref, err := ParseReference(poolRef, annotations)
	if err != nil {
		t.Fatalf("failed to parse reference: %v", err)
	}
	return ref
}

func selectorReference(t *testing.T, kind string, selector metav1.LabelSelector) Reference {
	t.Helper()
	encoded, err := selectorJSON(selector)
	if err != nil {
		t.Fatalf("failed to encode selector: %v", err)
	}
	return mustReference(t, ipamv1.IPPoolReference{
		APIGroup: v1alpha2.GroupVersion.Group,
		Kind:     kind,
		Name:     SelectorName,
	}, map[string]string{SelectorAnnotation: encoded})
}

func explicitReference(t *testing.T, kind, name string) Reference {
	t.Helper()
	return mustReference(t, ipamv1.IPPoolReference{
		APIGroup: v1alpha2.GroupVersion.Group,
		Kind:     kind,
		Name:     name,
	}, nil)
}

// TestSelect_DeclarationOutcome asserts, per declaration, exactly which pool
// is chosen or which error category is returned.
func TestSelect_DeclarationOutcome(t *testing.T) {
	envLabels := map[string]string{"env": "prod"}
	devLabels := map[string]string{"env": "dev"}

	// mkExplicit and mkSelector build a reference without pulling *testing.T
	// into the table literal, so each entry is plain data.
	mkExplicit := func(kind, name string) Reference {
		return Reference{Form: FormExplicit, Kind: SupportedKinds[kind], Name: name}
	}
	mkSelector := func(kind string, matchLabels map[string]string) Reference {
		selector := &metav1.LabelSelector{MatchLabels: matchLabels}
		compiled, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			t.Fatalf("failed to build selector: %v", err)
		}
		return Reference{Form: FormSelector, Kind: SupportedKinds[kind], Selector: compiled, RawSelector: selector}
	}

	tests := []struct {
		name       string
		ref        Reference
		candidates []Candidate
		wantPool   string
		wantScope  Scope
		wantKind   ErrorKind
	}{
		{
			name:       "only a global pool exists and is named explicitly",
			ref:        mkExplicit("GlobalInClusterIPPool", "shared"),
			candidates: []Candidate{globalIPPool("shared", nil)},
			wantPool:   "shared",
			wantScope:  ScopeCluster,
		},
		{
			name:       "only a namespaced pool exists and is named explicitly",
			ref:        mkExplicit("InClusterIPPool", "local"),
			candidates: []Candidate{namespacedIPPool("local", claimNamespace, nil)},
			wantPool:   "local",
			wantScope:  ScopeNamespaced,
		},
		{
			name:       "a namespaced and a global pool share a name; the kind selects the category",
			ref:        mkExplicit("GlobalInClusterIPPool", "same"),
			candidates: []Candidate{namespacedIPPool("same", claimNamespace, nil), globalIPPool("same", nil)},
			wantPool:   "same",
			wantScope:  ScopeCluster,
		},
		{
			name:       "the same name resolves to the namespaced pool when its kind is named",
			ref:        mkExplicit("InClusterIPPool", "same"),
			candidates: []Candidate{namespacedIPPool("same", claimNamespace, nil), globalIPPool("same", nil)},
			wantPool:   "same",
			wantScope:  ScopeNamespaced,
		},
		{
			name: "a selector prefers the namespaced pool over the global pool",
			ref:  mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{
				globalIPPool("global-prod", envLabels),
				namespacedIPPool("local-prod", claimNamespace, envLabels),
			},
			wantPool:  "local-prod",
			wantScope: ScopeNamespaced,
		},
		{
			name: "a selector falls back to the global pool when no namespaced pool matches",
			ref:  mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{
				globalIPPool("global-prod", envLabels),
				namespacedIPPool("local-dev", claimNamespace, devLabels),
			},
			wantPool:  "global-prod",
			wantScope: ScopeCluster,
		},
		{
			name: "two namespaced matches at the same level are ambiguous, not a guess",
			ref:  mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{
				namespacedIPPool("local-prod-a", claimNamespace, envLabels),
				namespacedIPPool("local-prod-b", claimNamespace, envLabels),
				globalIPPool("global-prod", envLabels),
			},
			wantKind: ErrAmbiguous,
		},
		{
			name: "two global matches at the same level are ambiguous",
			ref:  mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{
				globalIPPool("global-prod-a", envLabels),
				globalIPPool("global-prod-b", envLabels),
			},
			wantKind: ErrAmbiguous,
		},
		{
			name:       "an explicit pool that does not exist fails instead of falling back",
			ref:        mkExplicit("InClusterIPPool", "missing"),
			candidates: []Candidate{globalIPPool("missing", nil), namespacedIPPool("other", claimNamespace, nil)},
			wantKind:   ErrPoolNotFound,
		},
		{
			name:       "an explicit namespaced pool in another namespace is not visible",
			ref:        mkExplicit("InClusterIPPool", "local"),
			candidates: []Candidate{namespacedIPPool("local", "team-b", nil)},
			wantKind:   ErrPoolNotFound,
		},
		{
			name: "an exhausted pool still wins the selection",
			// Capacity is not a selection input: the chosen pool is reported as
			// exhausted by the allocation step instead of being skipped.
			ref: mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{
				namespacedIPPool("local-prod", claimNamespace, envLabels),
				globalIPPool("global-prod", envLabels),
			},
			wantPool:  "local-prod",
			wantScope: ScopeNamespaced,
		},
		{
			name:       "a selector that matches nothing does not use a default pool",
			ref:        mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{namespacedIPPool("local-dev", claimNamespace, devLabels)},
			wantKind:   ErrPoolNotFound,
		},
		{
			name:       "a terminating explicit pool is reported, not silently skipped",
			ref:        mkExplicit("InClusterIPPool", "going-away"),
			candidates: []Candidate{terminating(namespacedIPPool("going-away", claimNamespace, nil))},
			wantKind:   ErrPoolTerminating,
		},
		{
			name: "a selector that would pick a terminating pool reports it instead of moving on",
			ref:  mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{
				terminating(namespacedIPPool("local-prod", claimNamespace, envLabels)),
				globalIPPool("global-prod", envLabels),
			},
			wantKind: ErrPoolTerminating,
		},
		{
			name:       "an address selector never matches a prefix pool",
			ref:        mkSelector("InClusterIPPool", envLabels),
			candidates: []Candidate{namespacedPrefixPool("prefix-prod", claimNamespace, envLabels)},
			wantKind:   ErrPoolNotFound,
		},
		{
			name: "a global prefix pool satisfies a prefix selector",
			ref:  mkSelector("GlobalInClusterPrefixPool", envLabels),
			candidates: []Candidate{
				namespacedPrefixPool("prefix-dev", claimNamespace, devLabels),
				globalPrefixPool("prefix-prod", envLabels),
			},
			wantPool:  "prefix-prod",
			wantScope: ScopeCluster,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := Select(tt.ref, claimNamespace, tt.candidates)

			if tt.wantKind != "" {
				if err == nil {
					t.Fatalf("expected error %s, got pool %q", tt.wantKind, result.Object.GetName())
				}
				selectionErr, ok := err.(*Error)
				if !ok {
					t.Fatalf("expected a *Error, got %T: %v", err, err)
				}
				if selectionErr.Kind != tt.wantKind {
					t.Fatalf("expected error %s, got %s: %v", tt.wantKind, selectionErr.Kind, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := result.Object.GetName(); got != tt.wantPool {
				t.Fatalf("expected pool %q, got %q", tt.wantPool, got)
			}
			if result.Scope != tt.wantScope {
				t.Fatalf("expected scope %q, got %q", tt.wantScope, result.Scope)
			}
		})
	}
}

// TestParseReference_Forms documents which reference forms exist and how each
// is classified, including the unspecified default-pool path.
func TestParseReference_Forms(t *testing.T) {
	t.Run("an explicit name plus kind is an explicit reference", func(t *testing.T) {
		ref := explicitReference(t, "InClusterIPPool", "pool")
		if ref.Form != FormExplicit || ref.Name != "pool" || ref.Kind.Scope != ScopeNamespaced {
			t.Fatalf("unexpected reference: %+v", ref)
		}
	})

	t.Run("a selector marker plus annotation is a selector reference", func(t *testing.T) {
		ref := selectorReference(t, "InClusterIPPool", metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}})
		if ref.Form != FormSelector {
			t.Fatalf("expected selector form, got %q", ref.Form)
		}
		if !ref.Selector.Matches(labels.Set{"env": "prod"}) {
			t.Fatal("expected the selector to match env=prod")
		}
	})

	t.Run("an explicit name wins over a selector carried by the same reference", func(t *testing.T) {
		encoded, err := selectorJSON(metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}})
		if err != nil {
			t.Fatalf("failed to encode selector: %v", err)
		}
		ref := mustReference(t, ipamv1.IPPoolReference{
			APIGroup: v1alpha2.GroupVersion.Group,
			Kind:     "InClusterIPPool",
			Name:     "named",
		}, map[string]string{SelectorAnnotation: encoded})
		if ref.Form != FormExplicit || ref.Name != "named" {
			t.Fatalf("expected the explicit name to win, got %+v", ref)
		}
	})

	t.Run("no name and no selector is an invalid reference, never a default pool", func(t *testing.T) {
		_, err := ParseReference(ipamv1.IPPoolReference{
			APIGroup: v1alpha2.GroupVersion.Group,
			Kind:     "InClusterIPPool",
		}, nil)
		if !IsKind(err, ErrInvalidReference) {
			t.Fatalf("expected InvalidReference, got %v", err)
		}
	})

	t.Run("the selector marker without its annotation is an invalid reference", func(t *testing.T) {
		_, err := ParseReference(ipamv1.IPPoolReference{
			APIGroup: v1alpha2.GroupVersion.Group,
			Kind:     "InClusterIPPool",
			Name:     SelectorName,
		}, nil)
		if !IsKind(err, ErrInvalidReference) {
			t.Fatalf("expected InvalidReference, got %v", err)
		}
	})

	t.Run("an unknown kind is an invalid reference", func(t *testing.T) {
		_, err := ParseReference(ipamv1.IPPoolReference{
			APIGroup: v1alpha2.GroupVersion.Group,
			Kind:     "SomeOtherPool",
			Name:     "pool",
		}, nil)
		if !IsKind(err, ErrInvalidReference) {
			t.Fatalf("expected InvalidReference, got %v", err)
		}
	})

	t.Run("an unparsable selector annotation is an invalid reference", func(t *testing.T) {
		_, err := ParseReference(ipamv1.IPPoolReference{
			APIGroup: v1alpha2.GroupVersion.Group,
			Kind:     "InClusterIPPool",
			Name:     SelectorName,
		}, map[string]string{SelectorAnnotation: "not-json"})
		if !IsKind(err, ErrInvalidReference) {
			t.Fatalf("expected InvalidReference, got %v", err)
		}
	})
}

// TestExplicitNameBeatsSelector pins the precedence inside a single category so
// a later change cannot quietly make a selector override a stated name.
func TestExplicitNameBeatsSelector(t *testing.T) {
	labelled := map[string]string{"env": "prod"}
	ref := mustReference(t, ipamv1.IPPoolReference{
		APIGroup: v1alpha2.GroupVersion.Group,
		Kind:     "InClusterIPPool",
		Name:     "explicit",
	}, map[string]string{SelectorAnnotation: `{"matchLabels":{"env":"prod"}}`})
	result, err := Select(ref, claimNamespace, []Candidate{
		namespacedIPPool("explicit", claimNamespace, nil),
		namespacedIPPool("selected-by-labels", claimNamespace, labelled),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object.GetName() != "explicit" {
		t.Fatalf("expected the explicitly named pool, got %q", result.Object.GetName())
	}
}

// TestExistingReference_IsPinned verifies that an existing allocation is
// recognised from the pool recorded on the IPAddress, so that a label change or
// a new selection rule cannot migrate it.
func TestExistingReference_IsPinned(t *testing.T) {
	address := &ipamv1.IPAddress{
		Spec: ipamv1.IPAddressSpec{
			PoolRef: ipamv1.IPPoolReference{
				APIGroup: v1alpha2.GroupVersion.Group,
				Kind:     "InClusterIPPool",
				Name:     "original",
			},
		},
	}

	ref, ok := ExistingReference(address)
	if !ok {
		t.Fatal("expected the existing allocation to be recognised")
	}
	if ref.Form != FormExplicit || ref.Name != "original" {
		t.Fatalf("expected the original pool, got %+v", ref)
	}

	// Even if the claim's selector would now match a different pool, the
	// recorded reference still points at the original pool.
	candidates := []Candidate{
		namespacedIPPool("original", claimNamespace, nil),
		namespacedIPPool("better-match", claimNamespace, map[string]string{"env": "prod"}),
	}
	result, err := Select(ref, claimNamespace, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Object.GetName() != "original" {
		t.Fatalf("an existing allocation must not migrate; got %q", result.Object.GetName())
	}
}

func TestExistingReference_MissingOrUnsupported(t *testing.T) {
	if _, ok := ExistingReference(nil); ok {
		t.Fatal("nil address must not produce a reference")
	}
	if _, ok := ExistingReference(&ipamv1.IPAddress{}); ok {
		t.Fatal("address without a poolRef must not produce a reference")
	}
	if _, ok := ExistingReference(&ipamv1.IPAddress{
		Spec: ipamv1.IPAddressSpec{PoolRef: ipamv1.IPPoolReference{Kind: "Unknown", Name: "x"}},
	}); ok {
		t.Fatal("unsupported kind must not produce a reference")
	}
}

func TestKindsForFamily(t *testing.T) {
	addressKinds := KindsForFamily(FamilyAddress)
	if len(addressKinds) != 2 {
		t.Fatalf("expected 2 address kinds, got %d", len(addressKinds))
	}
	if addressKinds[0].Scope != ScopeNamespaced {
		t.Fatalf("expected the namespaced kind first, got %q", addressKinds[0].Scope)
	}
	for _, kind := range addressKinds {
		if kind.Family != FamilyAddress {
			t.Fatalf("unexpected family %q", kind.Family)
		}
	}

	prefixKinds := KindsForFamily(FamilyPrefix)
	if len(prefixKinds) != 2 || prefixKinds[0].Scope != ScopeNamespaced {
		t.Fatalf("unexpected prefix kinds: %+v", prefixKinds)
	}
}

func TestIsKind(t *testing.T) {
	if IsKind(nil, ErrAmbiguous) {
		t.Fatal("nil is not a selection error")
	}
	if IsKind(errors.New("boom"), ErrAmbiguous) {
		t.Fatal("a plain error is not a selection error")
	}
	if !IsKind(&Error{Kind: ErrAmbiguous}, ErrAmbiguous) {
		t.Fatal("expected the category to match")
	}
}
